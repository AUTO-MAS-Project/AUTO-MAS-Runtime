package macosprobe

import (
	"encoding/xml"
	"reflect"
	"strings"
	"testing"
)

func TestProbePlist_ExplicitSession(t *testing.T) {
	for _, session := range []string{"Background", "Aqua"} {
		t.Run(session, func(t *testing.T) {
			args := []string{"/tmp/目录 A&B/guard", "--", "arg<one>"}
			logPath := "/tmp/log A&B"
			source := probePlist("org.auto-mas.probe", args, logPath, session)
			var parsed struct {
				Dictionary struct {
					Strings []string `xml:"string"`
					Arrays  []struct {
						Strings []string `xml:"string"`
					} `xml:"array"`
				} `xml:"dict"`
			}
			if err := xml.Unmarshal([]byte(source), &parsed); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(parsed.Dictionary.Strings, []string{"org.auto-mas.probe", session, logPath, logPath}) {
				t.Fatalf("got strings %v, want explicit session and escaped values", parsed.Dictionary.Strings)
			}
			if len(parsed.Dictionary.Arrays) != 1 || !reflect.DeepEqual(parsed.Dictionary.Arrays[0].Strings, args) {
				t.Fatalf("got argument arrays %v, want %v", parsed.Dictionary.Arrays, args)
			}
			key := "<key>LimitLoadToSessionType</key><string>" + session + "</string>"
			if strings.Count(source, key) != 1 {
				t.Fatalf("got source %q, want one explicit session key", source)
			}
		})
	}
}
