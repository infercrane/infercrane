package proofgatefixture

import (
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"strings"
	"sync"

	"github.com/infercrane/infercrane/internal/optimizer"
)

// Server is a fake control plane for the proof gate. It serves every
// scenario's signed report under its run ID, and creates, approves, and
// cancels runs the way the control API answers. It never measures anything.
type Server struct {
	// Key signs every finished report; nil leaves them unsigned.
	Key ed25519.PrivateKey
	// APIKey, when set, is the only bearer credential accepted.
	APIKey string
	// OnCreate is the scenario a created run ends in.
	OnCreate string
	// RunningReads is how many reads of an approved run report it still
	// measuring before it reports OnCreate, so a caller's wait is exercised.
	RunningReads int

	mu        sync.Mutex
	proposals []optimizer.Proposal
	approved  map[string]float64
	cancelled []string
	reads     map[string]int
}

// Proposals returns the proposals of every run created so far.
func (s *Server) Proposals() []optimizer.Proposal {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]optimizer.Proposal(nil), s.proposals...)
}

// Approved returns the spend cap each approved run was given.
func (s *Server) Approved() map[string]float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]float64{}
	for id, cap := range s.approved {
		out[id] = cap
	}
	return out
}

// Cancelled returns the runs cancelled so far, in order.
func (s *Server) Cancelled() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.cancelled...)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.APIKey != "" && r.Header.Get("Authorization") != "Bearer "+s.APIKey {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]any{"code": "unauthorized", "message": "missing or invalid API key"}})
		return
	}
	const prefix = "/api/v1/optimization/campaigns"
	rest, found := strings.CutPrefix(r.URL.Path, prefix)
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"code": "not_found", "message": "not found"}})
		return
	}
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	switch {
	case r.Method == http.MethodPost && rest == "":
		s.create(w, r)
	case r.Method == http.MethodGet && len(parts) == 2 && parts[1] == "report":
		s.report(w, parts[0])
	case r.Method == http.MethodGet && len(parts) == 1 && parts[0] != "":
		s.campaign(w, parts[0], "")
	case r.Method == http.MethodPost && len(parts) == 2 && parts[1] == "approve":
		s.approve(w, r, parts[0])
	case r.Method == http.MethodPost && len(parts) == 2 && parts[1] == "cancel":
		s.mu.Lock()
		s.cancelled = append(s.cancelled, parts[0])
		s.mu.Unlock()
		s.campaign(w, parts[0], "cancelled")
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"code": "not_found", "message": "not found"}})
	}
}

func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Proposal         optimizer.Proposal `json:"proposal"`
		Intent           string             `json:"intent"`
		TargetDeployment string             `json:"target_deployment"`
	}
	if r.Header.Get("Idempotency-Key") == "" || json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&request) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"code": "invalid_request", "message": "Idempotency-Key and a JSON body are required"}})
		return
	}
	// The same checks the control API applies before it stores a run.
	if err := optimizer.ValidateProposal(request.Proposal); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": map[string]any{"code": "invalid_optimization_proposal", "message": err.Error()}})
		return
	}
	if err := optimizer.ValidateMeasurable(request.Proposal.Input); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": map[string]any{"code": "invalid_optimization_proposal", "message": err.Error()}})
		return
	}
	if request.Intent != "new_endpoint" || request.TargetDeployment != "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": map[string]any{"code": "invalid_optimization_intent", "message": "a two-setup comparison is a new_endpoint run"}})
		return
	}
	s.mu.Lock()
	s.proposals = append(s.proposals, request.Proposal)
	s.mu.Unlock()
	id := CampaignID(s.onCreate())
	writeJSON(w, http.StatusCreated, map[string]any{"campaign": s.view(id, "awaiting_approval", request.Proposal), "created": true, "provider_mutation": false})
}

func (s *Server) approve(w http.ResponseWriter, r *http.Request, id string) {
	var request struct {
		MaxCostUSD       float64 `json:"max_cost_usd"`
		ExpiresInSeconds int     `json:"expires_in_seconds"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&request) != nil || request.MaxCostUSD <= 0 || request.ExpiresInSeconds < 60 || request.ExpiresInSeconds > 86400 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": map[string]any{"code": "invalid_approval", "message": "max_cost_usd above zero and expires_in_seconds from 60 through 86400 are required"}})
		return
	}
	s.mu.Lock()
	if s.approved == nil {
		s.approved = map[string]float64{}
	}
	s.approved[id] = request.MaxCostUSD
	s.mu.Unlock()
	s.campaign(w, id, "approved")
}

func (s *Server) report(w http.ResponseWriter, id string) {
	scenario, known := ScenarioOf(id)
	if !known {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"code": "not_found", "message": "optimization run was not found"}})
		return
	}
	s.mu.Lock()
	_, created := s.approved[id]
	if s.reads == nil {
		s.reads = map[string]int{}
	}
	s.reads[id]++
	stillRunning := s.reads[id] <= s.RunningReads
	s.mu.Unlock()
	if created && stillRunning {
		scenario = Running
	}
	// Served under the requested ID, as the control plane would.
	document, err := DocumentAs(scenario, id, s.Key)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]any{"code": "internal", "message": err.Error()}})
		return
	}
	writeJSON(w, http.StatusOK, document)
}

func (s *Server) campaign(w http.ResponseWriter, id, state string) {
	if _, known := ScenarioOf(id); !known {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"code": "not_found", "message": "optimization run was not found"}})
		return
	}
	if state == "" {
		state = "running"
	}
	status := http.StatusOK
	if state == "cancelled" {
		status = http.StatusAccepted
	}
	writeJSON(w, status, map[string]any{"campaign": s.view(id, state, optimizer.Proposal{})})
}

func (s *Server) view(id, state string, proposal optimizer.Proposal) map[string]any {
	view := map[string]any{"id": id, "state": state, "model_identity": model, "objective": "interactive", "intent": "new_endpoint", "source": optimizer.ComparisonAlgorithmVersion, "candidates": []any{}}
	if proposal.Input.ModelIdentity != "" {
		view["model_identity"], view["objective"] = proposal.Input.ModelIdentity, proposal.Input.Objective
	}
	return view
}

func (s *Server) onCreate() string {
	if s.OnCreate == "" {
		return Pass
	}
	return s.OnCreate
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
