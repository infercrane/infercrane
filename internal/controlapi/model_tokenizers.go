package controlapi

import (
	"context"

	"github.com/infercrane/infercrane/internal/modelcheck"
	"github.com/infercrane/infercrane/internal/optimizer"
)

// maxRecordedTokenizers bounds the models whose tokenizer digest one
// proposal records (optimizer's model_tokenizers limit).
const maxRecordedTokenizers = 16

// withModelTokenizers records, in the request, the tokenizer digest of every
// model the proposal's setups serve when they serve more than one model (a
// published checkpoint beside the starting setup's model). A divergence
// between two models is read only when both digests are recorded and equal,
// so the stored proposal says why it was or was not read. It reads Hugging
// Face through the model check, public repositories only; a model it cannot
// read is left out, and its divergence is then unknown. Without the model
// check nothing is recorded. The digests are the server's own reading: a
// value a client sent is dropped, so a client cannot vouch that two
// tokenizers match.
func (a API) withModelTokenizers(ctx context.Context, request optimizer.Request, propose func(optimizer.Request) (optimizer.Proposal, error)) optimizer.Request {
	request.ModelTokenizers = nil
	if a.ModelCheck == nil {
		return request
	}
	proposal, err := propose(request)
	if err != nil || len(proposal.Candidates) < 2 {
		return request
	}
	control := proposal.Candidates[0].Deployment.Model
	type model struct{ id, revision string }
	var models []model
	seen := map[model]bool{}
	for _, candidate := range proposal.Candidates[1:] {
		served := candidate.Deployment.Model
		if served.ID == control.ID && served.Revision == control.Revision {
			continue
		}
		for _, item := range []model{{control.ID, control.Revision}, {served.ID, served.Revision}} {
			if item.revision != "" && modelcheck.ValidRepository(item.id) && !seen[item] && len(models) < maxRecordedTokenizers {
				seen[item] = true
				models = append(models, item)
			}
		}
	}
	tokenizers := map[string]string{}
	for _, item := range models {
		read, err := a.ModelCheck.Resolve(ctx, modelcheck.Request{Repository: item.id, Revision: item.revision})
		if err == nil && read.TokenizerDigest != "" {
			tokenizers[optimizer.TokenizerKey(item.id, item.revision)] = read.TokenizerDigest
		}
	}
	if len(tokenizers) > 0 {
		request.ModelTokenizers = tokenizers
	}
	return request
}
