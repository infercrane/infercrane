package supplieradapter

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

const (
	StreamEventContent = "content_delta"
	StreamEventFinish  = "finish"
	StreamEventUsage   = "usage"
)

type deepSeekStream struct {
	body      io.ReadCloser
	scanner   *bufio.Scanner
	requestID string
	data      []string
	done      bool
}

func newDeepSeekStream(body io.ReadCloser, requestID string) *deepSeekStream {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	return &deepSeekStream{body: body, scanner: scanner, requestID: requestID}
}

func (s *deepSeekStream) Next(ctx context.Context) (StreamEvent, error) {
	if s == nil || s.done {
		return StreamEvent{}, io.EOF
	}
	for {
		if err := ctx.Err(); err != nil {
			return StreamEvent{}, &Error{Code: ErrorCancelled, Message: "supplier stream was cancelled", Retry: RetryNever, Billing: BillingAmbiguous, ResponseStarted: true, Cause: err}
		}
		if !s.scanner.Scan() {
			if err := s.scanner.Err(); err != nil {
				s.done = true
				return StreamEvent{}, protocolStreamFailure("supplier stream framing failed", s.requestID, err)
			}
			if len(s.data) != 0 {
				return s.parseEvent()
			}
			s.done = true
			return StreamEvent{}, io.EOF
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

func (s *deepSeekStream) parseEvent() (StreamEvent, error) {
	payload := strings.Join(s.data, "\n")
	s.data = s.data[:0]
	if payload == "[DONE]" {
		s.done = true
		return StreamEvent{}, io.EOF
	}
	var raw struct {
		Model   string `json:"model"`
		Choices []struct {
			Index        int     `json:"index"`
			FinishReason *string `json:"finish_reason"`
			Delta        struct {
				Content *string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
		Usage *deepSeekUsage `json:"usage"`
	}
	if err := json.Unmarshal([]byte(payload), &raw); err != nil {
		s.done = true
		return StreamEvent{}, protocolStreamFailure("supplier stream contained malformed JSON", s.requestID, err)
	}
	if raw.Model != DeepSeekV4FlashModelID || len(raw.Choices) != 1 || raw.Choices[0].Index != 0 {
		s.done = true
		return StreamEvent{}, protocolStreamFailure("supplier stream did not match the qualified model contract", s.requestID, nil)
	}
	choice := raw.Choices[0]
	event := StreamEvent{Type: StreamEventContent, ChoiceIndex: choice.Index, SupplierRequestID: s.requestID}
	if choice.Delta.Content != nil {
		event.TextDelta = *choice.Delta.Content
	}
	if choice.FinishReason != nil {
		if !validDeepSeekFinishReason(*choice.FinishReason) {
			s.done = true
			return StreamEvent{}, protocolStreamFailure("supplier stream finish reason was invalid", s.requestID, nil)
		}
		event.Type = StreamEventFinish
		event.FinishReason = *choice.FinishReason
	}
	if raw.Usage != nil {
		usage := normalizeDeepSeekUsage(raw.Usage)
		if err := usage.Validate(); err != nil {
			s.done = true
			return StreamEvent{}, protocolStreamFailure("supplier stream contained invalid usage", s.requestID, err)
		}
		event.Usage = &usage
		if choice.Delta.Content == nil && choice.FinishReason == nil {
			event.Type = StreamEventUsage
		}
	}
	return event, nil
}

func (s *deepSeekStream) Close() error {
	if s == nil || s.body == nil {
		return nil
	}
	s.done = true
	return s.body.Close()
}

func protocolStreamFailure(message, requestID string, cause error) *Error {
	if cause != nil && errors.Is(cause, context.Canceled) {
		return &Error{Code: ErrorCancelled, Message: message, SupplierRequestID: requestID, Retry: RetryNever, Billing: BillingAmbiguous, ResponseStarted: true, Cause: cause}
	}
	return &Error{Code: ErrorProtocol, Message: message, SupplierRequestID: requestID, Retry: RetryNever, Billing: BillingAmbiguous, ResponseStarted: true, Cause: cause}
}
