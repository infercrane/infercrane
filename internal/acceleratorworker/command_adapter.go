package acceleratorworker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/acceleratorlab"
)

const defaultAdapterOutputBytes = 16 << 20

// CommandAdapter is the provider-neutral boundary between the worker API and
// an accelerator implementation. The executable receives one of `profile`,
// `generate`, or `qualify`, reads exactly one JSON request from stdin, and
// writes exactly one JSON evidence object to stdout. No shell is involved.
type CommandAdapter struct {
	executable       string
	prefixArgs       []string
	environment      []string
	maxOutputBytes   int
	operationTimeout time.Duration
}

type CommandAdapterConfig struct {
	Executable       string
	PrefixArgs       []string
	Environment      []string
	MaxOutputBytes   int
	OperationTimeout time.Duration
}

func NewCommandAdapter(config CommandAdapterConfig) (*CommandAdapter, error) {
	executable := filepath.Clean(strings.TrimSpace(config.Executable))
	if !filepath.IsAbs(executable) {
		return nil, errors.New("accelerator adapter executable must be absolute")
	}
	info, err := os.Lstat(executable)
	if err != nil {
		return nil, fmt.Errorf("inspect accelerator adapter executable: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return nil, errors.New("accelerator adapter must be an executable regular file")
	}
	maxOutput := config.MaxOutputBytes
	if maxOutput <= 0 {
		maxOutput = defaultAdapterOutputBytes
	}
	timeout := config.OperationTimeout
	if timeout <= 0 {
		timeout = 4 * time.Hour
	}
	return &CommandAdapter{
		executable: executable, prefixArgs: append([]string(nil), config.PrefixArgs...),
		environment: append([]string(nil), config.Environment...), maxOutputBytes: maxOutput,
		operationTimeout: timeout,
	}, nil
}

func (a *CommandAdapter) Capture(ctx context.Context, input acceleratorlab.ProfileIntent) (acceleratorlab.ProfileEvidence, error) {
	var output acceleratorlab.ProfileEvidence
	if err := a.call(ctx, "profile", input, &output); err != nil {
		return output, err
	}
	if output.InputDigest != input.InputDigest {
		return output, errors.New("profile evidence input digest mismatch")
	}
	return output, nil
}

func (a *CommandAdapter) Generate(ctx context.Context, input acceleratorlab.GenerationRequest) (acceleratorlab.GenerationEvidence, error) {
	var output acceleratorlab.GenerationEvidence
	if err := a.call(ctx, "generate", input, &output); err != nil {
		return output, err
	}
	if output.InputDigest != input.InputDigest || output.CandidateID != input.Candidate.ID {
		return output, errors.New("generation evidence identity mismatch")
	}
	return output, nil
}

func (a *CommandAdapter) Qualify(ctx context.Context, input acceleratorlab.QualificationRequest) (acceleratorlab.QualificationEvidence, error) {
	var output acceleratorlab.QualificationEvidence
	if err := a.call(ctx, "qualify", input, &output); err != nil {
		return output, err
	}
	if output.InputDigest != input.InputDigest {
		return output, errors.New("qualification evidence input digest mismatch")
	}
	return output, nil
}

func (a *CommandAdapter) call(ctx context.Context, operation string, input, output any) error {
	payload, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("encode %s request: %w", operation, err)
	}
	operationContext, cancel := context.WithTimeout(ctx, a.operationTimeout)
	defer cancel()
	arguments := append(append([]string(nil), a.prefixArgs...), operation)
	command := exec.CommandContext(operationContext, a.executable, arguments...)
	command.Env = append([]string(nil), a.environment...)
	command.Stdin = bytes.NewReader(payload)
	stdout := &boundedBuffer{remaining: a.maxOutputBytes}
	stderr := &boundedBuffer{remaining: 64 << 10}
	command.Stdout = stdout
	command.Stderr = stderr
	if err = command.Run(); err != nil {
		if errors.Is(operationContext.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("%s adapter deadline exceeded", operation)
		}
		stderrDigest := sha256.Sum256(stderr.Bytes())
		return fmt.Errorf("%s adapter failed: %w (stderr_sha256=%x stderr_bytes=%d)", operation, err, stderrDigest, len(stderr.Bytes()))
	}
	if stdout.truncated {
		return fmt.Errorf("%s adapter response exceeded %d bytes", operation, a.maxOutputBytes)
	}
	if err = decodeStrict(stdout.Bytes(), output); err != nil {
		return fmt.Errorf("decode %s evidence: %w", operation, err)
	}
	return nil
}

type boundedBuffer struct {
	buffer    bytes.Buffer
	remaining int
	truncated bool
}

func (b *boundedBuffer) Write(value []byte) (int, error) {
	original := len(value)
	if len(value) > b.remaining {
		value = value[:max(b.remaining, 0)]
		b.truncated = true
	}
	if len(value) > 0 {
		_, _ = b.buffer.Write(value)
		b.remaining -= len(value)
	}
	return original, nil
}

func (b *boundedBuffer) Bytes() []byte  { return b.buffer.Bytes() }
func (b *boundedBuffer) String() string { return b.buffer.String() }
