package logging

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func retainedFixture(
	t *testing.T,
	command string,
	date time.Time,
) fakeRetainedFile {
	t.Helper()
	layout := mustTestLayout(t)
	path, err := layout.RuntimeLogFile(command, date)
	if err != nil {
		t.Fatalf("RuntimeLogFile() error = %v", err)
	}
	return fakeRetainedFile{name: filepath.Base(path), path: path}
}

func candidateFixture(
	t *testing.T,
	layoutCommand string,
	date time.Time,
) retentionCandidate {
	t.Helper()
	layout := mustTestLayout(t)
	path, err := layout.RuntimeLogFile(layoutCommand, date)
	if err != nil {
		t.Fatalf("RuntimeLogFile() error = %v", err)
	}
	return retentionCandidate{
		file:      fakeRetainedFile{name: filepath.Base(path), path: path},
		command:   layoutCommand,
		localDate: date,
		pathKey:   retentionPathKey(path),
	}
}

func namesOf(files []retainedFile) []string {
	names := make([]string, 0, len(files))
	for _, file := range files {
		names = append(names, file.Name())
	}
	return names
}

func TestParseRetainedFile_AcceptsOnlyReconstructedLayoutPath(t *testing.T) {
	layout := mustTestLayout(t)
	location := time.FixedZone("CST", 8*60*60)
	date := time.Date(2026, 7, 9, 0, 0, 0, 0, location)
	path, err := layout.RuntimeLogFile("workspace-sync", date)
	if err != nil {
		t.Fatalf("RuntimeLogFile() error = %v", err)
	}
	aliasPath := switchDriveLetterCase(t, path)
	if aliasPath == path {
		t.Fatalf("switchDriveLetterCase() = input %q, want changed drive letter", path)
	}
	file := fakeRetainedFile{name: filepath.Base(path), path: aliasPath}
	got, ok := parseRetainedFile(layout, file, location)
	if !ok {
		t.Fatal("parseRetainedFile() ok = false, want true")
	}
	if got.command != "workspace-sync" || got.localDate.Format("20060102") != "20260709" {
		t.Fatalf("candidate = %#v, want workspace-sync/20260709", got)
	}
}

func TestParseRetainedFile_RejectsUnknownAndInvalidNames(t *testing.T) {
	layout := mustTestLayout(t)
	location := time.Local
	validPath, err := layout.RuntimeLogFile("doctor", time.Date(2026, 7, 9, 0, 0, 0, 0, location))
	if err != nil {
		t.Fatalf("RuntimeLogFile() error = %v", err)
	}
	tests := []fakeRetainedFile{
		{name: "unknown.txt", path: validPath},
		{name: "doctor-20260230.log", path: validPath},
		{name: "-20260709.log", path: validPath},
		{name: "doctor-20260709.log.extra", path: validPath},
		{name: filepath.Join("nested", "doctor-20260709.log"), path: validPath},
		{name: "doctor-20260709.log", path: filepath.Join(layout.RuntimeLogDir(), "other-20260709.log")},
	}
	for _, file := range tests {
		if got, ok := parseRetainedFile(layout, file, location); ok {
			t.Fatalf("parseRetainedFile(%#v) = %#v, want rejected", file, got)
		}
	}
}

func TestSelectRetentionRemovals_UsesLocalCalendarAge(t *testing.T) {
	location := time.FixedZone("CST", 8*60*60)
	now := time.Date(2026, 7, 30, 0, 30, 0, 0, location)
	layout := mustTestLayout(t)
	makeCandidate := func(day int) retentionCandidate {
		date := time.Date(2026, 7, day, 0, 0, 0, 0, location)
		path, err := layout.RuntimeLogFile("doctor", date)
		if err != nil {
			t.Fatalf("RuntimeLogFile(day %d) error = %v", day, err)
		}
		return retentionCandidate{
			file:    fakeRetainedFile{name: filepath.Base(path), path: path},
			command: "doctor", localDate: date, pathKey: retentionPathKey(path),
		}
	}
	candidates := []retentionCandidate{makeCandidate(1), makeCandidate(2), makeCandidate(30)}
	got := namesOf(selectRetentionRemovals(
		candidates, candidates[2].file.Path(), now,
		RetentionPolicy{MaxAgeDays: 29, MaxFilesPerCommand: 30},
	))
	want := []string{"doctor-20260701.log"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("removals = %#v, want %#v", got, want)
	}
}

func TestSelectRetentionRemovals_AppliesCountPerCommand(t *testing.T) {
	location := time.Local
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, location)
	layout := mustTestLayout(t)
	var candidates []retentionCandidate
	for _, command := range []string{"doctor", "workspace-sync"} {
		for day := 27; day <= 30; day++ {
			date := time.Date(2026, 7, day, 0, 0, 0, 0, location)
			path, err := layout.RuntimeLogFile(command, date)
			if err != nil {
				t.Fatalf("RuntimeLogFile() error = %v", err)
			}
			candidates = append(candidates, retentionCandidate{
				file:    fakeRetainedFile{name: filepath.Base(path), path: path},
				command: command, localDate: date, pathKey: retentionPathKey(path),
			})
		}
	}
	got := namesOf(selectRetentionRemovals(
		candidates, "", now,
		RetentionPolicy{MaxAgeDays: 30, MaxFilesPerCommand: 2},
	))
	want := []string{
		"doctor-20260727.log",
		"doctor-20260728.log",
		"workspace-sync-20260727.log",
		"workspace-sync-20260728.log",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("removals = %#v, want %#v", got, want)
	}
}

func TestSelectRetentionRemovals_ProtectsActiveAndCountsFutureFiles(t *testing.T) {
	location := time.Local
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, location)
	layout := mustTestLayout(t)
	dates := []time.Time{
		time.Date(2026, 7, 1, 0, 0, 0, 0, location),
		time.Date(2026, 7, 29, 0, 0, 0, 0, location),
		time.Date(2026, 7, 30, 0, 0, 0, 0, location),
		time.Date(2026, 8, 2, 0, 0, 0, 0, location),
	}
	candidates := make([]retentionCandidate, 0, len(dates))
	for _, date := range dates {
		path, err := layout.RuntimeLogFile("doctor", date)
		if err != nil {
			t.Fatalf("RuntimeLogFile() error = %v", err)
		}
		candidates = append(candidates, retentionCandidate{
			file:    fakeRetainedFile{name: filepath.Base(path), path: path},
			command: "doctor", localDate: date, pathKey: retentionPathKey(path),
		})
	}
	got := namesOf(selectRetentionRemovals(
		candidates,
		candidates[0].file.Path(),
		now,
		RetentionPolicy{MaxAgeDays: 30, MaxFilesPerCommand: 2},
	))
	want := []string{"doctor-20260729.log", "doctor-20260730.log"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("removals = %#v, want %#v", got, want)
	}
}

func TestSelectRetentionRemovals_IsDeterministic(t *testing.T) {
	location := time.Local
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, location)
	layout := mustTestLayout(t)
	makeCandidate := func(command string, suffix string) retentionCandidate {
		date := time.Date(2026, 7, 1, 0, 0, 0, 0, location)
		path, err := layout.RuntimeLogFile(command, date)
		if err != nil {
			t.Fatalf("RuntimeLogFile() error = %v", err)
		}
		path = filepath.Join(filepath.Dir(path), suffix+"-"+filepath.Base(path))
		return retentionCandidate{
			file:    fakeRetainedFile{name: filepath.Base(path), path: path},
			command: command, localDate: date, pathKey: retentionPathKey(path),
		}
	}
	input := []retentionCandidate{
		makeCandidate("zeta", "b"),
		makeCandidate("alpha", "b"),
		makeCandidate("alpha", "a"),
		makeCandidate("zeta", "a"),
	}
	first := namesOf(selectRetentionRemovals(
		input, "", now,
		RetentionPolicy{MaxAgeDays: 1, MaxFilesPerCommand: 30},
	))
	second := namesOf(selectRetentionRemovals(
		[]retentionCandidate{input[2], input[0], input[3], input[1]}, "", now,
		RetentionPolicy{MaxAgeDays: 1, MaxFilesPerCommand: 30},
	))
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("removals differ by input order: %#v vs %#v", first, second)
	}
}

func switchDriveLetterCase(t *testing.T, path string) string {
	t.Helper()
	if len(path) < 2 || path[1] != ':' {
		t.Fatalf("path = %q, want drive-absolute path", path)
	}
	drive := path[0]
	var switched byte
	switch {
	case drive >= 'A' && drive <= 'Z':
		switched = drive + ('a' - 'A')
	case drive >= 'a' && drive <= 'z':
		switched = drive - ('a' - 'A')
	default:
		t.Fatalf("drive letter = %q, want ASCII letter", drive)
	}
	alias := string(switched) + path[1:]
	if alias == path {
		t.Fatalf("drive-case alias = input %q, want changed spelling", path)
	}
	return alias
}
