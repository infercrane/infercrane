package supplieradapter

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

type zaiStream struct {
	body          io.ReadCloser
	scanner       *bufio.Scanner
	requestID     string
	expectedModel string
	responseID    string
	data          []string
	done          bool
	sawFinish     bool
	sawUsage      bool
}

func newZAIStream(body io.ReadCloser, requestID, expectedModel string) *zaiStream {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64<<10), ZAIMVPMaxStreamEventBytes)
	return &zaiStream{body: body, scanner: scanner, requestID: requestID, expectedModel: expectedModel}
}

func (s *zaiStream) Next(ctx context.Context) (StreamEvent, error) {
	if s == nil || s.done {
		return StreamEvent{}, io.EOF
	}
	for {
		if err := ctx.Err(); err != nil {
			s.done = true
			return StreamEvent{}, zaiProtocolStreamFailure("supplier stream was cancelled", s.requestID, err)
		}
		if !s.scanner.Scan() {
			if err := s.scanner.Err(); err != nil {
				s.done = true
				return StreamEvent{}, zaiProtocolStreamFailure("supplier stream framing failed", s.requestID, err)
			}
			if len(s.data) != 0 {
				return s.parseEvent()
			}
			s.done = true
			return StreamEvent{}, zaiProtocolStreamFailure("supplier stream ended before its terminal marker", s.requestID, io.ErrUnexpectedEOF)
		}
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

func (s *zaiStream) parseEvent() (StreamEvent, error) {
	payload := strings.Join(s.data, "\n")
	s.data = s.data[:0]
	if payload == "[DONE]" {
		s.done = true
		if !s.sawFinish || !s.sawUsage {
			return StreamEvent{}, zaiProtocolStreamFailure("supplier stream ended without a complete terminal result", s.requestID, nil)
		}
		return StreamEvent{}, io.EOF
	}
	var raw struct {
		ID        string `json:"id"`
		RequestID string `json:"request_id"`
		Model     string `json:"model"`
		Choices   []struct {
			Index        int     `json:"index"`
			FinishReason *string `json:"finish_reason"`
			Delta        struct {
				Content *string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
		Usage *zaiUsage `json:"usage"`
	}
	if err := json.Unmarshal([]byte(payload), &raw); err != nil {
		s.done = true
		return StreamEvent{}, zaiProtocolStreamFailure("supplier stream contained malformed JSON", s.requestID, err)
	}
	requestID, err := mergeZAIRequestID(s.requestID, raw.RequestID)
	if err != nil {
		s.done = true
		return StreamEvent{}, zaiProtocolStreamFailure("supplier stream request identity did not match", s.requestID, err)
	}
	s.requestID = requestID
	if strings.TrimSpace(raw.ID) == "" || raw.Model != s.expectedModel || len(raw.Choices) != 1 || raw.Choices[0].Index != 0 {
		s.done = true
		return StreamEvent{}, zaiProtocolStreamFailure("supplier stream did not match the qualified model contract", s.requestID, nil)
	}
	if s.responseID == "" {
		s.responseID = raw.ID
	} else if s.responseID != raw.ID {
		s.done = true
		return StreamEvent{}, zaiProtocolStreamFailure("supplier stream response identity changed", s.requestID, nil)
	}
	choice := raw.Choices[0]
	event := StreamEvent{Type: StreamEventContent, ChoiceIndex: choice.Index, SupplierRequestID: s.requestID}
	if choice.Delta.Content != nil {
		event.TextDelta = *choice.Delta.Content
	}
	if choice.FinishReason != nil {
		finishReason, valid := normalizeZAIFinishReason(choice.FinishReason)
		if !valid || s.sawFinish {
			s.done = true
			return StreamEvent{}, zaiProtocolStreamFailure("supplier stream finish reason was invalid", s.requestID, nil)
		}
		s.sawFinish = true
		event.Type = StreamEventFinish
		event.FinishReason = finishReason
	}
	if raw.Usage != nil {
		usage := normalizeZAIUsage(raw.Usage)
		if err := validateZAIUsage(usage); err != nil || usage.State != UsageComplete || s.sawUsage {
			s.done = true
			return StreamEvent{}, zaiProtocolStreamFailure("supplier stream contained invalid terminal usage", s.requestID, err)
		}
		s.sawUsage = true
		event.Usage = &usage
		if choice.Delta.Content == nil && choice.FinishReason == nil {
			event.Type = StreamEventUsage
		}
	}
	if s.sawUsage && !s.sawFinish {
		s.done = true
		return StreamEvent{}, zaiProtocolStreamFailure("supplier stream reported usage before finishing", s.requestID, nil)
	}
	return event, nil
}

func (s *zaiStream) Close() error {
	if s == nil || s.body == nil {
		return nil
	}
	s.done = true
	return s.body.Close()
}

func zaiProtocolStreamFailure(message, requestID string, cause error) *Error {
	code := ErrorProtocol
	if cause != nil && errors.Is(cause, context.Canceled) {
		code = ErrorCancelled
	}
	return &Error{Code: code, Message: message, SupplierRequestID: requestID, Retry: RetryNever, Billing: BillingAmbiguous, ResponseStarted: true, Cause: cause}
}
