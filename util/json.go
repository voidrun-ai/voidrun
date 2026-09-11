package util

import (
	"encoding/json"
	"fmt"
	"io"
)

// DecodeStrictJSON unmarshals JSON and rejects unknown fields.
func DecodeStrictJSON(r io.Reader, dst any) error {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("unexpected extra JSON")
	}
	return nil
}
