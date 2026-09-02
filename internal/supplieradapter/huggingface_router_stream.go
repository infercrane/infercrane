package supplieradapter

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

type huggingFaceRouterStream struct {
	body      io.ReadCloser
	scanner   *bufio.Scanner
	requestID string
	expected  huggingFaceExpectedModel
	response  string
	data      []string
	finished  bool
	usageSeen bool
	done      bool
}

func newHuggingFaceRouterStream(body io.ReadCloser, requestID string, expected huggingFaceExpectedModel) *huggingFaceRouterStream {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64<<10), HuggingFaceRouterMaxStreamEventBytes)
	return &huggingFaceRouterStream{body: body, scanner: scanner, requestID: requestID, expected: expected}
}

func (s *huggingFaceRouterStream) Next(ctx context.Context) (StreamEvent, error) {
	if s == nil || s.done {
		return StreamEvent{}, io.EOF
	}
	for {
		if err := ctx.Err(); err != nil {
			s.done = true
			return StreamEvent{}, huggingFaceStreamFailure("supplier stream was cancelled", s.requestID, err)
		}
		if !s.scanner.Scan() {
			if err := s.scanner.Err(); err != nil {
				s.done = true
				return StreamEvent{}, huggingFaceStreamFailure("supplier stream framing failed", s.requestID, err)
			}
			if len(s.data) != 0 {
				return s.parseEvent()
			}
			s.done = true
			return StreamEvent{}, huggingFaceStreamFailure("supplier stream ended before its terminal marker", s.requestID, io.ErrUnexpectedEOF)
		}
		// bufio.Scanner removes '\n' but preserves the '\r' in valid CRLF SSE
		// framing. Normalize only that framing byte; payload data remains exact.
		line := strings.TrimSuffix(s.scanner.Text(), "\r")
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

func (s *huggingFaceRouterStream) parseEvent() (StreamEvent, error) {
	payload := strings.Join(s.data, "\n")
	s.data = s.data[:0]
	if payload == "[DONE]" {
		s.done = true
		if !s.finished || !s.usageSeen {
			return StreamEvent{}, huggingFaceStreamFailure("supplier stream ended without a finish reason and complete usage", s.requestID, nil)
		}
		return StreamEvent{}, io.EOF
	}
	if s.usageSeen {
		s.done = true
		return StreamEvent{}, huggingFaceStreamFailure("supplier stream continued after terminal usage", s.requestID, nil)
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
		Usage *huggingFaceUsage `json:"usage"`
	}
	if err := json.Unmarshal([]byte(payload), &raw); err != nil {
		s.done = true
		return StreamEvent{}, huggingFaceStreamFailure("supplier stream contained malformed JSON", s.requestID, err)
	}
	if strings.TrimSpace(raw.ID) == "" || !s.expected.accepts(raw.Model) {
		s.done = true
		return StreamEvent{}, huggingFaceStreamFailure("supplier stream did not match the qualified model contract", s.requestID, nil)
	}
	if s.response == "" {
		s.response = raw.ID
	} else if raw.ID != s.response {
		s.done = true
		return StreamEvent{}, huggingFaceStreamFailure("supplier stream response identity changed", s.requestID, nil)
	}

	event := StreamEvent{Type: StreamEventContent, SupplierRequestID: s.requestID}
	if len(raw.Choices) > 1 {
		s.done = true
		return StreamEvent{}, huggingFaceStreamFailure("supplier stream returned multiple choices", s.requestID, nil)
	}
	if len(raw.Choices) == 1 {
		if s.finished {
			s.done = true
			return StreamEvent{}, huggingFaceStreamFailure("supplier stream continued after its finish reason", s.requestID, nil)
		}
		choice := raw.Choices[0]
		if choice.Index != 0 {
			s.done = true
			return StreamEvent{}, huggingFaceStreamFailure("supplier stream choice index was invalid", s.requestID, nil)
		}
		event.ChoiceIndex = choice.Index
		if choice.Delta.Content != nil {
			event.TextDelta = *choice.Delta.Content
		}
		if choice.FinishReason != nil {
			if !validHuggingFaceFinishReason(*choice.FinishReason) {
				s.done = true
				return StreamEvent{}, huggingFaceStreamFailure("supplier stream finish reason was invalid", s.requestID, nil)
			}
			s.finished = true
			event.Type = StreamEventFinish
			event.FinishReason = *choice.FinishReason
		}
	} else if raw.Usage == nil {
		s.done = true
		return StreamEvent{}, huggingFaceStreamFailure("supplier stream event had neither a choice nor usage", s.requestID, nil)
	}

	if raw.Usage != nil {
		usage := normalizeHuggingFaceUsage(raw.Usage)
		if len(raw.Choices) != 0 || !s.finished {
			s.done = true
			return StreamEvent{}, huggingFaceStreamFailure("supplier stream usage was not terminal", s.requestID, nil)
		}
		if err := validateHuggingFaceUsage(usage); err != nil || usage.State != UsageComplete {
			s.done = true
			return StreamEvent{}, huggingFaceStreamFailure("supplier stream contained invalid terminal usage", s.requestID, err)
		}
		s.usageSeen = true
		event.Type = StreamEventUsage
		event.Usage = &usage
	}
	return event, nil
}

func (s *huggingFaceRouterStream) Close() error {
	if s == nil || s.body == nil {
		return nil
	}
	s.done = true
	return s.body.Close()
}

func huggingFaceStreamFailure(message, requestID string, cause error) *Error {
	code := ErrorProtocol
	if errors.Is(cause, context.Canceled) {
		code = ErrorCancelled
	} else if errors.Is(cause, context.DeadlineExceeded) {
		code = ErrorTimeout
	}
	return &Error{Code: code, Message: message, SupplierRequestID: requestID, Retry: RetryNever, Billing: BillingAmbiguous, ResponseStarted: true, Cause: cause}
}
