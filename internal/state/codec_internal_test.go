package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

func mustStateJSON(t *testing.T, value any) []byte {
	t.Helper()
	payload, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatalf("json.MarshalIndent() error = %v", err)
	}
	return append(payload, '\n')
}

func TestCodec_RejectsMalformedDuplicateUnknownAndMissingFields(t *testing.T) {
	t.Parallel()

	valid := validTransactionState(TransactionMutation)
	validPayload := mustStateJSON(t, valid)
	var object map[string]any
	if err := json.Unmarshal(validPayload, &object); err != nil {
		t.Fatalf("json.Unmarshal(valid) error = %v", err)
	}
	delete(object, "operationId")
	missing := mustStateJSON(t, object)

	if err := json.Unmarshal(validPayload, &object); err != nil {
		t.Fatalf("json.Unmarshal(valid) error = %v", err)
	}
	object["secretValue"] = "must-not-leak"
	unknown := mustStateJSON(t, object)

	tests := []struct {
		name    string
		payload []byte
	}{
		{name: "empty", payload: nil},
		{name: "null", payload: []byte("null\n")},
		{name: "array", payload: []byte("[]\n")},
		{name: "truncated", payload: []byte(`{"schemaVersion":1` + "\n")},
		{name: "duplicate_top", payload: []byte(`{"schemaVersion":1,"pid":1,"pid":2}` + "\n")},
		{
			name: "duplicate_nested",
			payload: []byte(
				`{"schemaVersion":1,"status":"ready_to_start",` +
					`"updatedAt":"2026-07-29T01:02:03Z",` +
					`"lastSuccessful":{"version":"v1","version":"v2",` +
					`"commit":"0123456789abcdef0123456789abcdef01234567"},` +
					`"broken":null}` + "\n",
			),
		},
		{name: "missing", payload: missing},
		{name: "unknown", payload: unknown},
		{name: "semantic", payload: mustStateJSON(t, func() TransactionState {
			value := valid
			value.PID = 0
			return value
		}())},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			_, err := decodeTransaction(
				"mutation",
				TransactionMutation,
				test.payload,
			)
			if !errors.Is(err, ErrCorrupt) {
				t.Fatalf("decodeTransaction() error = %v, want ErrCorrupt", err)
			}
			if strings.Contains(err.Error(), "secretValue") ||
				strings.Contains(err.Error(), "must-not-leak") {
				t.Fatalf("error leaked untrusted JSON text: %v", err)
			}
		})
	}
}

func TestCodec_RejectsCaseVariantKeys(t *testing.T) {
	t.Parallel()

	transaction := validTransactionState(TransactionMutation)
	transactionPayload := mustStateJSON(t, transaction)
	var transactionObject map[string]any
	if err := json.Unmarshal(transactionPayload, &transactionObject); err != nil {
		t.Fatalf("json.Unmarshal(transaction) error = %v", err)
	}

	operationIDVariant := make(map[string]any, len(transactionObject))
	for key, value := range transactionObject {
		operationIDVariant[key] = value
	}
	delete(operationIDVariant, "operationId")
	operationIDVariant["operationID"] = transaction.OperationID

	pidAlias := make(map[string]any, len(transactionObject)+1)
	for key, value := range transactionObject {
		pidAlias[key] = value
	}
	pidAlias["PID"] = transaction.PID + 1

	environment := EnvironmentState{
		SchemaVersion:  SchemaVersion,
		Status:         protocol.StateReadyToStart,
		UpdatedAt:      fixedStateTime().UTC(),
		LastSuccessful: validLastSuccessful(),
		Broken:         nil,
	}
	environmentPayload := mustStateJSON(t, environment)
	var environmentObject map[string]any
	if err := json.Unmarshal(environmentPayload, &environmentObject); err != nil {
		t.Fatalf("json.Unmarshal(environment) error = %v", err)
	}
	lastSuccessful, ok := environmentObject["lastSuccessful"].(map[string]any)
	if !ok {
		t.Fatalf("lastSuccessful type = %T, want object", environmentObject["lastSuccessful"])
	}
	delete(lastSuccessful, "version")
	lastSuccessful["Version"] = environment.LastSuccessful.Version

	tests := []struct {
		name   string
		decode func() error
	}{
		{
			name: "transaction_field",
			decode: func() error {
				_, err := decodeTransaction(
					"mutation",
					TransactionMutation,
					mustStateJSON(t, operationIDVariant),
				)
				return err
			},
		},
		{
			name: "transaction_alias_duplicate",
			decode: func() error {
				_, err := decodeTransaction(
					"mutation",
					TransactionMutation,
					mustStateJSON(t, pidAlias),
				)
				return err
			},
		},
		{
			name: "nested_environment_field",
			decode: func() error {
				_, err := decodeEnvironment(
					mustTestLayout(t),
					"environment",
					mustStateJSON(t, environmentObject),
				)
				return err
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			if err := test.decode(); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("decode case-variant key error = %v, want ErrCorrupt", err)
			}
		})
	}
}

func TestCodec_RejectsDuplicateKeysBeforeSchemaClassification(t *testing.T) {
	t.Parallel()

	transactionPayload := mustStateJSON(
		t,
		validTransactionState(TransactionMutation),
	)
	transactionDuplicate := strings.Replace(
		string(transactionPayload),
		`"pid":`,
		`"pid": 1, "pid":`,
		1,
	)

	ready := EnvironmentState{
		SchemaVersion:  SchemaVersion,
		Status:         protocol.StateReadyToStart,
		UpdatedAt:      fixedStateTime().UTC(),
		LastSuccessful: validLastSuccessful(),
		Broken:         nil,
	}
	readyDuplicate := strings.Replace(
		string(mustStateJSON(t, ready)),
		`"version":`,
		`"version": "v9", "version":`,
		1,
	)

	store := newTestStore(t, completeFakeStateFiles(), fixedStateTime)
	broken, err := store.NewBrokenEnvironment(
		validLastSuccessful(),
		validOperationFailed(store),
	)
	if err != nil {
		t.Fatalf("NewBrokenEnvironment() error = %v", err)
	}
	brokenDuplicate := strings.Replace(
		string(mustStateJSON(t, broken)),
		`"reason":`,
		`"reason": "repository_changed", "reason":`,
		1,
	)

	tests := []struct {
		name   string
		decode func() error
	}{
		{
			name: "complete_transaction_top_level",
			decode: func() error {
				_, err := decodeTransaction(
					"mutation",
					TransactionMutation,
					[]byte(transactionDuplicate),
				)
				return err
			},
		},
		{
			name: "complete_environment_last_successful",
			decode: func() error {
				_, err := decodeEnvironment(
					mustTestLayout(t),
					"environment",
					[]byte(readyDuplicate),
				)
				return err
			},
		},
		{
			name: "complete_environment_broken",
			decode: func() error {
				_, err := decodeEnvironment(
					mustTestLayout(t),
					"environment",
					[]byte(brokenDuplicate),
				)
				return err
			},
		},
		{
			name: "unsupported_schema_deep_duplicate",
			decode: func() error {
				_, err := decodeTransaction(
					"mutation",
					TransactionMutation,
					[]byte(
						`{"schemaVersion":2,"future":[{"value":1,"value":2}]}`+
							"\n",
					),
				)
				return err
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			err := test.decode()
			if !errors.Is(err, ErrCorrupt) ||
				!errors.Is(err, errDuplicateField) ||
				errors.Is(err, ErrUnsupportedSchema) {
				t.Fatalf(
					"duplicate-key error = %v, want corrupt duplicate before unsupported",
					err,
				)
			}
		})
	}
}

func TestCodec_RequiresEveryTransactionAndEnvironmentField(t *testing.T) {
	t.Parallel()

	transaction := validTransactionState(TransactionMutation)
	transactionPayload := mustStateJSON(t, transaction)
	var transactionObject map[string]any
	if err := json.Unmarshal(transactionPayload, &transactionObject); err != nil {
		t.Fatalf("json.Unmarshal(transaction) error = %v", err)
	}
	for _, field := range []string{
		"schemaVersion",
		"operationId",
		"command",
		"pid",
		"startedAt",
		"targetVersion",
		"stage",
	} {
		field := field
		t.Run("transaction_"+field, func(t *testing.T) {
			copyOfObject := make(map[string]any, len(transactionObject))
			for key, value := range transactionObject {
				copyOfObject[key] = value
			}
			delete(copyOfObject, field)
			_, err := decodeTransaction(
				"mutation",
				TransactionMutation,
				mustStateJSON(t, copyOfObject),
			)
			if !errors.Is(err, ErrCorrupt) {
				t.Fatalf("missing %s error = %v, want ErrCorrupt", field, err)
			}
		})
	}

	environment := EnvironmentState{
		SchemaVersion:  SchemaVersion,
		Status:         protocol.StateReadyToStart,
		UpdatedAt:      fixedStateTime().UTC(),
		LastSuccessful: validLastSuccessful(),
		Broken:         nil,
	}
	environmentPayload := mustStateJSON(t, environment)
	var environmentObject map[string]any
	if err := json.Unmarshal(environmentPayload, &environmentObject); err != nil {
		t.Fatalf("json.Unmarshal(environment) error = %v", err)
	}
	for _, field := range []string{
		"schemaVersion",
		"status",
		"updatedAt",
		"lastSuccessful",
		"broken",
	} {
		field := field
		t.Run("environment_"+field, func(t *testing.T) {
			copyOfObject := make(map[string]any, len(environmentObject))
			for key, value := range environmentObject {
				copyOfObject[key] = value
			}
			delete(copyOfObject, field)
			_, err := decodeEnvironment(
				mustTestLayout(t),
				"environment",
				mustStateJSON(t, copyOfObject),
			)
			if !errors.Is(err, ErrCorrupt) {
				t.Fatalf("missing %s error = %v, want ErrCorrupt", field, err)
			}
		})
	}

	for _, field := range []string{"version", "commit"} {
		field := field
		t.Run("lastSuccessful_"+field, func(t *testing.T) {
			var object map[string]any
			if err := json.Unmarshal(environmentPayload, &object); err != nil {
				t.Fatalf("json.Unmarshal(environment) error = %v", err)
			}
			lastSuccessful := object["lastSuccessful"].(map[string]any)
			delete(lastSuccessful, field)
			if _, err := decodeEnvironment(
				mustTestLayout(t),
				"environment",
				mustStateJSON(t, object),
			); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("missing lastSuccessful.%s error = %v, want ErrCorrupt", field, err)
			}
		})
	}

	store := newTestStore(t, completeFakeStateFiles(), fixedStateTime)
	repositoryChanged, err := store.NewBrokenEnvironment(
		validLastSuccessful(),
		validRepositoryChanged(store),
	)
	if err != nil {
		t.Fatalf("NewBrokenEnvironment() error = %v", err)
	}
	brokenPayload := mustStateJSON(t, repositoryChanged)
	for _, field := range []string{
		"targetVersion",
		"branch",
		"commit",
		"pythonVersion",
		"uvVersion",
		"reason",
		"stage",
		"exitCode",
		"logPath",
	} {
		field := field
		t.Run("broken_"+field, func(t *testing.T) {
			var object map[string]any
			if err := json.Unmarshal(brokenPayload, &object); err != nil {
				t.Fatalf("json.Unmarshal(broken environment) error = %v", err)
			}
			broken := object["broken"].(map[string]any)
			delete(broken, field)
			if _, err := decodeEnvironment(
				mustTestLayout(t),
				"environment",
				mustStateJSON(t, object),
			); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("missing broken.%s error = %v, want ErrCorrupt", field, err)
			}
		})
	}
}

func TestCodec_DistinguishesUnsupportedSchemaBeforeV1Validation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		payload []byte
		want    error
	}{
		{
			name:    "missing",
			payload: []byte(`{"pid":1}` + "\n"),
			want:    ErrCorrupt,
		},
		{
			name:    "string",
			payload: []byte(`{"schemaVersion":"1"}` + "\n"),
			want:    ErrCorrupt,
		},
		{
			name:    "fraction",
			payload: []byte(`{"schemaVersion":1.0}` + "\n"),
			want:    ErrCorrupt,
		},
		{
			name:    "unsupported_despite_missing_v1_fields",
			payload: []byte(`{"schemaVersion":2}` + "\n"),
			want:    ErrUnsupportedSchema,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			_, err := decodeTransaction(
				"backend",
				TransactionBackend,
				test.payload,
			)
			if !errors.Is(err, test.want) {
				t.Fatalf("decodeTransaction() error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestCodec_RejectsBOMInvalidUTF8AndNonCanonicalTail(t *testing.T) {
	t.Parallel()

	valid := mustStateJSON(t, validTransactionState(TransactionBackend))
	withoutNewline := append([]byte(nil), valid[:len(valid)-1]...)
	withBOM := append([]byte{0xef, 0xbb, 0xbf}, valid...)
	invalidUTF8 := append([]byte(nil), valid...)
	invalidUTF8[1] = 0xff
	withSecondValue := append(append([]byte(nil), valid...), []byte("{}\n")...)
	withBlankLine := append(append([]byte(nil), valid...), '\n')
	withCRLF := append(append([]byte(nil), valid[:len(valid)-1]...), '\r', '\n')

	tests := []struct {
		name    string
		payload []byte
	}{
		{name: "missing_newline", payload: withoutNewline},
		{name: "bom", payload: withBOM},
		{name: "invalid_utf8", payload: invalidUTF8},
		{name: "second_value", payload: withSecondValue},
		{name: "blank_line", payload: withBlankLine},
		{name: "crlf", payload: withCRLF},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			_, err := decodeTransaction(
				"backend",
				TransactionBackend,
				test.payload,
			)
			if !errors.Is(err, ErrCorrupt) {
				t.Fatalf("decodeTransaction() error = %v, want ErrCorrupt", err)
			}
		})
	}

	oversized := bytes.Repeat([]byte{' '}, int(maxStateFileBytes)+1)
	if _, err := inspectStateJSON(oversized); !errors.Is(err, errStateFileTooLarge) {
		t.Fatalf("inspectStateJSON(oversized) error = %v, want size error", err)
	}
}
