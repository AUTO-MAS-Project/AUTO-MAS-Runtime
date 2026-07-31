package logging

import (
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

type retentionCandidate struct {
	file      retainedFile
	command   string
	localDate time.Time
	pathKey   string
	active    bool
}

func parseRetainedFile(
	layout *config.Layout,
	file retainedFile,
	location *time.Location,
) (retentionCandidate, bool) {
	if layout == nil || file == nil || location == nil {
		return retentionCandidate{}, false
	}
	name := file.Name()
	if filepath.Base(name) != name || !strings.HasSuffix(name, ".log") {
		return retentionCandidate{}, false
	}
	stem := strings.TrimSuffix(name, ".log")
	if len(stem) <= 9 || stem[len(stem)-9] != '-' {
		return retentionCandidate{}, false
	}
	command := stem[:len(stem)-9]
	dateText := stem[len(stem)-8:]
	localDate, err := time.ParseInLocation("20060102", dateText, location)
	if err != nil {
		return retentionCandidate{}, false
	}
	expected, err := layout.RuntimeLogFile(command, localDate)
	if err != nil || !sameRetentionPath(expected, file.Path()) {
		return retentionCandidate{}, false
	}
	return retentionCandidate{
		file:      file,
		command:   command,
		localDate: localDate,
		pathKey:   retentionPathKey(file.Path()),
	}, true
}

func selectRetentionRemovals(
	candidates []retentionCandidate,
	activePath string,
	now time.Time,
	policy RetentionPolicy,
) []retainedFile {
	work := append([]retentionCandidate(nil), candidates...)
	selected := make([]bool, len(work))
	location := now.Location()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, location)
	oldestKept := today.AddDate(0, 0, -(policy.MaxAgeDays - 1))

	for index := range work {
		work[index].active = sameRetentionPath(work[index].file.Path(), activePath)
		if !work[index].active && work[index].localDate.Before(oldestKept) {
			selected[index] = true
		}
	}

	byCommand := make(map[string][]int)
	for index := range work {
		if selected[index] {
			continue
		}
		byCommand[work[index].command] = append(byCommand[work[index].command], index)
	}
	for _, indexes := range byCommand {
		sort.Slice(indexes, func(left, right int) bool {
			a := work[indexes[left]]
			b := work[indexes[right]]
			if !a.localDate.Equal(b.localDate) {
				return a.localDate.After(b.localDate)
			}
			return a.pathKey < b.pathKey
		})
		overflow := len(indexes) - policy.MaxFilesPerCommand
		for position := len(indexes) - 1; position >= 0 && overflow > 0; position-- {
			index := indexes[position]
			if work[index].active {
				continue
			}
			selected[index] = true
			overflow--
		}
	}

	removals := make([]retentionCandidate, 0)
	for index, remove := range selected {
		if remove {
			removals = append(removals, work[index])
		}
	}
	sort.Slice(removals, func(left, right int) bool {
		if removals[left].command != removals[right].command {
			return removals[left].command < removals[right].command
		}
		if !removals[left].localDate.Equal(removals[right].localDate) {
			return removals[left].localDate.Before(removals[right].localDate)
		}
		return removals[left].pathKey < removals[right].pathKey
	})
	files := make([]retainedFile, 0, len(removals))
	for _, candidate := range removals {
		files = append(files, candidate.file)
	}
	return files
}

func sameRetentionPath(left, right string) bool {
	if left == "" || right == "" {
		return false
	}
	return strings.EqualFold(filepath.Clean(left), filepath.Clean(right))
}

func retentionPathKey(path string) string {
	return strings.ToLower(strings.ReplaceAll(filepath.Clean(path), "/", `\`))
}
