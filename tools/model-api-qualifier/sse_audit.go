package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"strings"
)

type sseWireAudit struct {
	body       io.ReadCloser
	maximum    int64
	hash       hash.Hash
	read       int64
	pending    string
	doneFrames int
	invalid    bool
	deferred   error
}

func newSSEWireAudit(body io.ReadCloser, maximum int64) *sseWireAudit {
	return &sseWireAudit{body: body, maximum: maximum, hash: sha256.New()}
}

func (a *sseWireAudit) Read(value []byte) (int, error) {
	if a.deferred != nil {
		err := a.deferred
		a.deferred = nil
		return 0, err
	}
	n, err := a.body.Read(value)
	if n > 0 {
		a.read += int64(n)
		if a.read > a.maximum {
			a.invalid = true
			return 0, errors.New("stream byte limit exceeded")
		}
		_, _ = a.hash.Write(value[:n])
		a.consume(string(value[:n]))
		if err != nil {
			a.deferred = err
		}
		return n, nil
	}
	if err == io.EOF {
		a.finishLine()
	}
	return n, err
}

func (a *sseWireAudit) consume(fragment string) {
	a.pending += fragment
	if len(a.pending) > 1<<20 {
		a.invalid = true
		return
	}
	for {
		index := strings.IndexByte(a.pending, '\n')
		if index < 0 {
			return
		}
		a.consumeLine(strings.TrimSuffix(a.pending[:index], "\r"))
		a.pending = a.pending[index+1:]
	}
}

func (a *sseWireAudit) finishLine() {
	if a.pending != "" {
		a.consumeLine(strings.TrimSuffix(a.pending, "\r"))
		a.pending = ""
	}
}

func (a *sseWireAudit) consumeLine(line string) {
	if line == "data: [DONE]" {
		a.doneFrames++
	}
}

func (a *sseWireAudit) Validate() error {
	if a.invalid || a.doneFrames != 1 {
		return errors.New("SSE stream requires exactly one bounded terminal DONE frame")
	}
	return nil
}

func (a *sseWireAudit) Close() error     { return a.body.Close() }
func (a *sseWireAudit) Digest() string   { return hex.EncodeToString(a.hash.Sum(nil)) }
func (a *sseWireAudit) BytesRead() int64 { return a.read }
func (a *sseWireAudit) DoneFrames() int  { return a.doneFrames }
