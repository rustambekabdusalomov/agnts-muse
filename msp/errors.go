package msp

import (
	"encoding/json"
	"errors"
	"fmt"
)

// JSON-RPC codes this client produces itself.
const (
	codeMethodNotFound = -32601
	codeInternal       = -32603
)

// Connection errors.
var (
	// ErrClosed: the connection ended (the server exited or Close was called).
	ErrClosed = errors.New("msp: connection closed")
	// ErrTooLarge: the server sent a line over the size limit.
	ErrTooLarge = errors.New("msp: message too large")
	// ErrSchemaMismatch: the server speaks a different MSP schema version.
	ErrSchemaMismatch = errors.New("msp: unsupported schema version")
)

// Error is an MSP error response. MSP puts a machine-readable kind in the
// error data ("notInitialized", "invalidParams", "approvalRequirementStale",
// …); compare with errors.Is against the Err* kinds below.
type Error struct {
	Code      int             `json:"code"`
	Message   string          `json:"message"`
	Data      json.RawMessage `json:"data,omitempty"`
	Kind      string          `json:"-"`
	Retryable bool            `json:"-"`
}

func (e *Error) Error() string {
	if e.Kind != "" {
		return fmt.Sprintf("msp: %s (%d): %s", e.Kind, e.Code, e.Message)
	}
	return fmt.Sprintf("msp: error %d: %s", e.Code, e.Message)
}

// Is matches errors of the same kind, so errors.Is(err, msp.ErrNotInitialized)
// works for any notInitialized response.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Kind != "" && t.Kind == e.Kind
}

// UnmarshalJSON reads the kind and retryable flag out of data.
func (e *Error) UnmarshalJSON(b []byte) error {
	type plain Error
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*e = Error(p)
	var data struct {
		Kind      string `json:"kind"`
		Retryable bool   `json:"retryable"`
	}
	if len(e.Data) > 0 && json.Unmarshal(e.Data, &data) == nil {
		e.Kind, e.Retryable = data.Kind, data.Retryable
	}
	return nil
}

// MarshalJSON writes the kind back into data.
func (e *Error) MarshalJSON() ([]byte, error) {
	type plain Error
	p := plain(*e)
	if e.Kind != "" && len(p.Data) == 0 {
		p.Data, _ = json.Marshal(map[string]any{"kind": e.Kind, "retryable": e.Retryable})
	}
	return json.Marshal(p)
}

// Error kinds from the schema's error table.
var (
	ErrNotInitialized   = &Error{Kind: "notInitialized"}
	ErrInvalidParams    = &Error{Kind: "invalidParams"}
	ErrMethodNotFound   = &Error{Kind: "methodNotFound"}
	ErrSessionNotFound  = &Error{Kind: "sessionNotFound"}
	ErrTurnNotActive    = &Error{Kind: "turnNotActive"}
	ErrApprovalStale    = &Error{Kind: "approvalRequirementStale"}
	ErrApprovalResolved = &Error{Kind: "approvalAlreadyResolved"}
)
