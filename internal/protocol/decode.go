// Frame codec. Decode turns one WebSocket text frame into the typed message
// for its "type"; Encode is the inverse. Both directions are covered so the
// gateway test server can speak the protocol with the same code.
package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// UnknownTypeError is returned by Decode for a well-formed frame whose type
// this build does not know. The doc says to answer it with an error message
// and keep the socket open.
type UnknownTypeError struct{ Type string }

func (e *UnknownTypeError) Error() string { return fmt.Sprintf("unknown message type %q", e.Type) }

// ErrNoType is returned for a frame without a "type" field.
var ErrNoType = errors.New("frame has no type")

// Decode parses a frame. The result is a pointer to one of Hello, Welcome,
// Reject, Ping, Pong, Bye, ErrorMsg, QueryReq or QueryRes.
func Decode(data []byte) (any, error) {
	var env struct {
		Type    string `json:"type"`
		QueryID string `json:"query_id"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("unparseable frame: %w", err)
	}
	switch env.Type {
	case TypeHello:
		return decodeInto[Hello](data)
	case TypeWelcome:
		return decodeInto[Welcome](data)
	case TypeReject:
		return decodeInto[Reject](data)
	case TypePing:
		return decodeInto[Ping](data)
	case TypePong:
		return decodeInto[Pong](data)
	case TypeBye:
		return decodeInto[Bye](data)
	case TypeError:
		return decodeInto[ErrorMsg](data)
	case TypeQueryRes:
		return decodeInto[QueryRes](data)
	case TypeQueryReq:
		var q QueryReq
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.UseNumber()
		if err := dec.Decode(&q); err != nil {
			// Keep the id so the answer can name the request.
			q = QueryReq{Type: TypeQueryReq, QueryID: env.QueryID, decodeProblem: "malformed request: " + err.Error()}
			return &q, nil
		}
		for k, v := range q.Values {
			q.Values[k] = NormalizeNumbers(v)
		}
		for k, v := range q.Set {
			q.Set[k] = NormalizeNumbers(v)
		}
		return &q, nil
	case "":
		return nil, ErrNoType
	default:
		return nil, &UnknownTypeError{Type: env.Type}
	}
}

func decodeInto[T any](data []byte) (*T, error) {
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("malformed frame: %w", err)
	}
	return &v, nil
}

// Encode marshals a message for the wire.
func Encode(v any) ([]byte, error) { return json.Marshal(v) }
