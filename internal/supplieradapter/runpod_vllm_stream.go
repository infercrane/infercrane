package supplieradapter

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

type runPodVLLMStream struct {
	body          io.ReadCloser
	scanner       *bufio.Scanner
	requestID     string
	expectedModel string
	responseID    string
	data          []string
	finished      bool
	usageSeen     bool
	done          bool
}

func newRunPodVLLMStream(body io.ReadCloser, requestID, expectedModel string) *runPodVLLMStream {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64<<10), RunPodVLLMMaxStreamEventBytes)
	return &runPodVLLMStream{body: body, scanner: scanner, requestID: requestID, expectedModel: expectedModel}
}

func (s *runPodVLLMStream) Next(ctx context.Context) (StreamEvent, error) {
	if s == nil || s.done {
		return StreamEvent{}, io.EOF
	}
	for {
		if err := ctx.Err(); err != nil {
			s.done = true
			return StreamEvent{}, runPodStreamFailure("supplier stream was cancelled", s.requestID, err)
		}
		if !s.scanner.Scan() {
			if err := s.scanner.Err(); err != nil {
				s.done = true
				return StreamEvent{}, runPodStreamFailure("supplier stream framing failed", s.requestID, err)
			}
			if len(s.data) != 0 {
				return s.parseEvent()
			}
			s.done = true
			return StreamEvent{}, runPodStreamFailure("supplier stream ended before its terminal marker", s.requestID, io.ErrUnexpectedEOF)
		}
		line := s.scanner.Text()
		if line == "" {
			if len(s.data) == 0 {
				continue
			}
			return s.parseEvent()
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if line == "data" {
			s.data = append(s.data, "")
			continue
		}
		if strings.HasPrefix(line, "data:") {
			s.data = append(s.data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
}

func (s *runPodVLLMStream) parseEvent() (StreamEvent, error) {
	payload := strings.Join(s.data, "\n")
	s.data = s.data[:0]
	if payload == "[DONE]" {
		s.done = true
		if !s.finished || !s.usageSeen {
			return StreamEvent{}, runPodStreamFailure("supplier stream ended without a finish reason and complete usage", s.requestID, nil)
		}
		return StreamEvent{}, io.EOF
	}
	if s.usageSeen {
		s.done = true
		return StreamEvent{}, runPodStreamFailure("supplier stream continued after terminal usage", s.requestID, nil)
	}
	var raw struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Index        int     `json:"index"`
			FinishReason *string `json:"finish_reason"`
			Delta        struct {
				Content *string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
		Usage *runPodVLLMUsage `json:"usage"`
	}
	if err := json.Unmarshal([]byte(payload), &raw); err != nil {
		s.done = true
		return StreamEvent{}, runPodStreamFailure("supplier stream contained malformed JSON", s.requestID, err)
	}
	if strings.TrimSpace(raw.ID) == "" || raw.Model != s.expectedModel {
		s.done = true
		return StreamEvent{}, runPodStreamFailure("supplier stream did not match the qualified model contract", s.requestID, nil)
	}
	if s.responseID == "" {
		s.responseID = raw.ID
	} else if raw.ID != s.responseID {
		s.done = true
		return StreamEvent{}, runPodStreamFailure("supplier stream response identity changed", s.requestID, nil)
	}

	event := StreamEvent{Type: StreamEventContent, SupplierRequestID: s.requestID}
	if len(raw.Choices) > 1 {
		s.done = true
		return StreamEvent{}, runPodStreamFailure("supplier stream returned multiple choices", s.requestID, nil)
	}
	if len(raw.Choices) == 1 {
		if s.finished {
			s.done = true
			return StreamEvent{}, runPodStreamFailure("supplier stream continued after its finish reason", s.requestID, nil)
		}
		choice := raw.Choices[0]
		if choice.Index != 0 {
			s.done = true
			return StreamEvent{}, runPodStreamFailure("supplier stream choice index was invalid", s.requestID, nil)
		}
		event.ChoiceIndex = choice.Index
		if choice.Delta.Content != nil {
			event.TextDelta = *choice.Delta.Content
		}
		if choice.FinishReason != nil {
			if !validRunPodFinishReason(*choice.FinishReason) || s.finished {
				s.done = true
				return StreamEvent{}, runPodStreamFailure("supplier stream finish reason was invalid", s.requestID, nil)
			}
			s.finished = true
			event.Type = StreamEventFinish
			event.FinishReason = *choice.FinishReason
		}
	} else if raw.Usage == nil {
		s.done = true
		return StreamEvent{}, runPodStreamFailure("supplier stream event had neither a choice nor usage", s.requestID, nil)
	}

	if raw.Usage != nil {
		usage := normalizeRunPodVLLMUsage(raw.Usage)
		if len(raw.Choices) != 0 || !s.finished {
			s.done = true
			return StreamEvent{}, runPodStreamFailure("supplier stream usage was not terminal", s.requestID, nil)
		}
		if err := validateRunPodVLLMUsage(usage); err != nil || usage.State != UsageComplete {
			s.done = true
			return StreamEvent{}, runPodStreamFailure("supplier stream contained invalid or duplicate terminal usage", s.requestID, err)
		}
		s.usageSeen = true
		event.Usage = &usage
		event.Type = StreamEventUsage
	}
	return event, nil
}

func (s *runPodVLLMStream) Close() error {
	if s == nil || s.body == nil {
		return nil
	}
	s.done = true
	return s.body.Close()
}

func runPodStreamFailure(message, requestID string, cause error) *Error {
	code := ErrorProtocol
	if errors.Is(cause, context.Canceled) {
		code = ErrorCancelled
	} else if errors.Is(cause, context.DeadlineExceeded) {
		code = ErrorTimeout
	}
	return &Error{Code: code, Message: message, SupplierRequestID: requestID, Retry: RetryNever, Billing: BillingAmbiguous, ResponseStarted: true, Cause: cause}
}
