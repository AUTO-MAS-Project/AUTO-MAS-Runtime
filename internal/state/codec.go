package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

const maxStateFileBytes = filesystem.MaxStateFileBytes

type transactionWire struct {
	SchemaVersion *int            `json:"schemaVersion"`
	OperationID   *string         `json:"operationId"`
	Command       *string         `json:"command"`
	PID           *uint32         `json:"pid"`
	StartedAt     *jsonTime       `json:"startedAt"`
	TargetVersion *string         `json:"targetVersion"`
	Stage         *protocol.Stage `json:"stage"`
}

type environmentWire struct {
	SchemaVersion  *int                  `json:"schemaVersion"`
	Status         *protocol.StateStatus `json:"status"`
	UpdatedAt      *jsonTime             `json:"updatedAt"`
	LastSuccessful *revisionWire         `json:"lastSuccessful"`
	Broken         json.RawMessage       `json:"broken"`
}

type revisionWire struct {
	Version *string `json:"version"`
	Commit  *string `json:"commit"`
}

type brokenEnvironmentWire struct {
	TargetVersion *string         `json:"targetVersion"`
	Branch        *string         `json:"branch"`
	Commit        *string         `json:"commit"`
	PythonVersion *string         `json:"pythonVersion"`
	UVVersion     *string         `json:"uvVersion"`
	Reason        *BrokenReason   `json:"reason"`
	Stage         *protocol.Stage `json:"stage"`
	ExitCode      *int            `json:"exitCode"`
	LogPath       *string         `json:"logPath"`
}

type jsonTime = time.Time

func inspectStateJSON(payload []byte) (int, error) {
	if len(payload) == 0 || int64(len(payload)) > maxStateFileBytes {
		if int64(len(payload)) > maxStateFileBytes {
			return 0, errStateFileTooLarge
		}
		return 0, errInvalidJSON
	}
	if !utf8.Valid(payload) || bytes.HasPrefix(payload, []byte{0xef, 0xbb, 0xbf}) {
		return 0, errInvalidJSON
	}
	if payload[len(payload)-1] != '\n' {
		return 0, errInvalidJSON
	}
	body := payload[:len(payload)-1]
	if len(body) == 0 || body[len(body)-1] != '}' {
		return 0, errInvalidJSON
	}

	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	first, err := decoder.Token()
	if err != nil {
		return 0, errInvalidJSON
	}
	open, ok := first.(json.Delim)
	if !ok || open != '{' {
		return 0, errInvalidJSON
	}
	schemaVersion, schemaPresent, err := scanJSONObject(decoder, true)
	if err != nil {
		return 0, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return 0, errInvalidJSON
	}
	if !schemaPresent {
		return 0, errMissingField
	}
	return schemaVersion, nil
}

func scanJSONObject(
	decoder *json.Decoder,
	captureSchema bool,
) (int, bool, error) {
	seen := make(map[string]struct{})
	schemaVersion := 0
	schemaPresent := false
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return 0, false, errInvalidJSON
		}
		key, ok := keyToken.(string)
		if !ok {
			return 0, false, errInvalidJSON
		}
		if _, exists := seen[key]; exists {
			return 0, false, errDuplicateField
		}
		seen[key] = struct{}{}

		valueToken, err := decoder.Token()
		if err != nil {
			return 0, false, errInvalidJSON
		}
		if captureSchema && key == "schemaVersion" {
			number, ok := valueToken.(json.Number)
			if !ok {
				return 0, false, errInvalidJSON
			}
			parsed, err := strconv.ParseInt(
				number.String(),
				10,
				strconv.IntSize,
			)
			if err != nil {
				return 0, false, errInvalidJSON
			}
			schemaVersion = int(parsed)
			schemaPresent = true
		}
		if err := scanJSONValue(decoder, valueToken); err != nil {
			return 0, false, err
		}
	}
	closing, err := decoder.Token()
	if err != nil {
		return 0, false, errInvalidJSON
	}
	delim, ok := closing.(json.Delim)
	if !ok || delim != '}' {
		return 0, false, errInvalidJSON
	}
	return schemaVersion, schemaPresent, nil
}

func scanJSONValue(decoder *json.Decoder, token json.Token) error {
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		_, _, err := scanJSONObject(decoder, false)
		return err
	case '[':
		for decoder.More() {
			item, err := decoder.Token()
			if err != nil {
				return errInvalidJSON
			}
			if err := scanJSONValue(decoder, item); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return errInvalidJSON
		}
		closeDelim, ok := closing.(json.Delim)
		if !ok || closeDelim != ']' {
			return errInvalidJSON
		}
		return nil
	default:
		return errInvalidJSON
	}
}

func decodeTransaction(
	file string,
	kind TransactionKind,
	payload []byte,
) (TransactionState, error) {
	if err := checkSchema(file, payload); err != nil {
		return TransactionState{}, err
	}
	if err := validateExactJSONKeys(payload, transactionJSONShape()); err != nil {
		return TransactionState{}, corrupt(file, errInvalidJSON)
	}
	var wire transactionWire
	if err := decodeStrict(payload, &wire); err != nil {
		return TransactionState{}, corrupt(file, errInvalidJSON)
	}
	value, err := wire.transaction()
	if err != nil {
		return TransactionState{}, corrupt(file, err)
	}
	if err := ValidateTransaction(kind, value); err != nil {
		return TransactionState{}, corrupt(file, err)
	}
	return value, nil
}

func decodeEnvironment(
	layout *config.Layout,
	file string,
	payload []byte,
) (EnvironmentState, error) {
	if err := checkSchema(file, payload); err != nil {
		return EnvironmentState{}, err
	}
	if err := validateExactJSONKeys(payload, environmentJSONShape()); err != nil {
		return EnvironmentState{}, corrupt(file, errInvalidJSON)
	}
	var wire environmentWire
	if err := decodeStrict(payload, &wire); err != nil {
		return EnvironmentState{}, corrupt(file, errInvalidJSON)
	}
	value, err := wire.environment()
	if err != nil {
		return EnvironmentState{}, corrupt(file, err)
	}
	if err := validateEnvironment(layout, value); err != nil {
		return EnvironmentState{}, corrupt(file, err)
	}
	return value, nil
}

func checkSchema(file string, payload []byte) error {
	schemaVersion, err := inspectStateJSON(payload)
	if err != nil {
		return corrupt(file, err)
	}
	if schemaVersion != SchemaVersion {
		return &UnsupportedSchemaError{File: file, Got: schemaVersion}
	}
	return nil
}

type jsonObjectShape map[string]jsonObjectShape

func transactionJSONShape() jsonObjectShape {
	return jsonObjectShape{
		"schemaVersion": nil,
		"operationId":   nil,
		"command":       nil,
		"pid":           nil,
		"startedAt":     nil,
		"targetVersion": nil,
		"stage":         nil,
	}
}

func environmentJSONShape() jsonObjectShape {
	return jsonObjectShape{
		"schemaVersion": nil,
		"status":        nil,
		"updatedAt":     nil,
		"lastSuccessful": {
			"version": nil,
			"commit":  nil,
		},
		"broken": {
			"targetVersion": nil,
			"branch":        nil,
			"commit":        nil,
			"pythonVersion": nil,
			"uvVersion":     nil,
			"reason":        nil,
			"stage":         nil,
			"exitCode":      nil,
			"logPath":       nil,
		},
	}
}

func validateExactJSONKeys(payload []byte, shape jsonObjectShape) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(payload, &object); err != nil || object == nil {
		return errInvalidJSON
	}
	for key, raw := range object {
		nested, ok := shape[key]
		if !ok {
			return errInvalidJSON
		}
		if nested == nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			continue
		}
		if err := validateExactJSONKeys(raw, nested); err != nil {
			return errInvalidJSON
		}
	}
	return nil
}

func decodeStrict(payload []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errInvalidJSON
	}
	return nil
}

func (w transactionWire) transaction() (TransactionState, error) {
	switch {
	case w.SchemaVersion == nil:
		return TransactionState{}, missing("schemaVersion")
	case w.OperationID == nil:
		return TransactionState{}, missing("operationId")
	case w.Command == nil:
		return TransactionState{}, missing("command")
	case w.PID == nil:
		return TransactionState{}, missing("pid")
	case w.StartedAt == nil:
		return TransactionState{}, missing("startedAt")
	case w.TargetVersion == nil:
		return TransactionState{}, missing("targetVersion")
	case w.Stage == nil:
		return TransactionState{}, missing("stage")
	}
	return TransactionState{
		SchemaVersion: *w.SchemaVersion,
		OperationID:   *w.OperationID,
		Command:       *w.Command,
		PID:           *w.PID,
		StartedAt:     time.Time(*w.StartedAt),
		TargetVersion: *w.TargetVersion,
		Stage:         *w.Stage,
	}, nil
}

func (w environmentWire) environment() (EnvironmentState, error) {
	switch {
	case w.SchemaVersion == nil:
		return EnvironmentState{}, missing("schemaVersion")
	case w.Status == nil:
		return EnvironmentState{}, missing("status")
	case w.UpdatedAt == nil:
		return EnvironmentState{}, missing("updatedAt")
	case w.LastSuccessful == nil:
		return EnvironmentState{}, missing("lastSuccessful")
	case w.Broken == nil:
		return EnvironmentState{}, missing("broken")
	}
	revision, err := w.LastSuccessful.revision()
	if err != nil {
		return EnvironmentState{}, err
	}
	broken, err := decodeBrokenWire(w.Broken)
	if err != nil {
		return EnvironmentState{}, err
	}
	return EnvironmentState{
		SchemaVersion:  *w.SchemaVersion,
		Status:         *w.Status,
		UpdatedAt:      time.Time(*w.UpdatedAt),
		LastSuccessful: revision,
		Broken:         broken,
	}, nil
}

func (w revisionWire) revision() (Revision, error) {
	switch {
	case w.Version == nil:
		return Revision{}, missing("lastSuccessful.version")
	case w.Commit == nil:
		return Revision{}, missing("lastSuccessful.commit")
	default:
		return Revision{Version: *w.Version, Commit: *w.Commit}, nil
	}
}

func decodeBrokenWire(payload json.RawMessage) (*BrokenEnvironment, error) {
	if bytes.Equal(bytes.TrimSpace(payload), []byte("null")) {
		return nil, nil
	}
	var wire brokenEnvironmentWire
	if err := decodeStrict(payload, &wire); err != nil {
		return nil, errInvalidJSON
	}
	switch {
	case wire.TargetVersion == nil:
		return nil, missing("targetVersion")
	case wire.Branch == nil:
		return nil, missing("branch")
	case wire.Commit == nil:
		return nil, missing("commit")
	case wire.PythonVersion == nil:
		return nil, missing("pythonVersion")
	case wire.UVVersion == nil:
		return nil, missing("uvVersion")
	case wire.Reason == nil:
		return nil, missing("reason")
	case wire.Stage == nil:
		return nil, missing("stage")
	case wire.ExitCode == nil:
		return nil, missing("exitCode")
	case wire.LogPath == nil:
		return nil, missing("logPath")
	}
	return &BrokenEnvironment{
		TargetVersion: *wire.TargetVersion,
		Branch:        *wire.Branch,
		Commit:        *wire.Commit,
		PythonVersion: *wire.PythonVersion,
		UVVersion:     *wire.UVVersion,
		Reason:        *wire.Reason,
		Stage:         *wire.Stage,
		ExitCode:      *wire.ExitCode,
		LogPath:       *wire.LogPath,
	}, nil
}

func corrupt(file string, cause error) error {
	return &CorruptError{File: file, Cause: cause}
}

func missing(field string) error {
	return &ValidationError{Field: field, Cause: errMissingField}
}
