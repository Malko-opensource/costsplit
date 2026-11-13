package bench

import (
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// Location scoring, for repositories whose tests cannot be run here: an
// instance passes when the agent edited every file the reference patch
// edits, within locateNear lines of one of its hunks. It says the agent
// found the place, not that the fix is right.

const locateNear = 15

type span struct{ lo, hi int }

// Location is where an agent's edits landed against the reference patch.
type Location struct {
	GoldFiles  int      `json:"gold_files"`
	FilesHit   int      `json:"files_hit"`  // reference files the agent edited
	FilesNear  int      `json:"files_near"` // of those, edited near a reference hunk
	GoldHunks  int      `json:"gold_hunks"`
	HunksNear  int      `json:"hunks_near"`
	Edited     []string `json:"edited"`
	ExtraFiles int      `json:"extra_files"` // files edited that the reference patch leaves alone
}

func (l Location) Hit() bool { return l.GoldFiles > 0 && l.FilesNear == l.GoldFiles }

func (l Location) String() string {
	return fmt.Sprintf("files %d/%d, near a reference hunk %d/%d, hunks %d/%d, other files edited %d",
		l.FilesHit, l.GoldFiles, l.FilesNear, l.GoldFiles, l.HunksNear, l.GoldHunks, l.ExtraFiles)
}

var (
	locHunk = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+\d+(?:,\d+)? @@`)
	oldFile = regexp.MustCompile(`^--- (?:a/)?(.+)$`)
)

// oldSpans reads a unified diff and returns, per old-side path, the line
// ranges of the original file each hunk changes (context lines excluded).
func oldSpans(diff string, strip string) map[string][]span {
	out := map[string][]span{}
	path, line := "", 0
	open := false
	flush := func() { open = false }
	for _, ln := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(ln, "--- "):
			if m := oldFile.FindStringSubmatch(ln); m != nil {
				path = strings.TrimPrefix(strings.TrimPrefix(m[1], strip), "/")
				if m[1] == "/dev/null" {
					path = "" // a new file has no place in the original
				}
			}
			flush()
		case strings.HasPrefix(ln, "+++ "), strings.HasPrefix(ln, "diff "):
			flush()
		case strings.HasPrefix(ln, "@@"):
			if m := locHunk.FindStringSubmatch(ln); m != nil {
				line, _ = strconv.Atoi(m[1])
			}
			flush()
		case strings.HasPrefix(ln, "-"), strings.HasPrefix(ln, "+"):
			if path == "" {
				continue
			}
			if !open {
				out[path] = append(out[path], span{line, line})
				open = true
			}
			if ln[0] == '-' {
				out[path][len(out[path])-1].hi = line
				line++
			}
		case strings.HasPrefix(ln, " "):
			line++
			flush()
		}
	}
	return out
}

// Locate compares the working copy with the pristine checkout.
func (s *SWE) Locate(env *SWEEnv, work string) Location {
	gold := oldSpans(s.gold, "")
	raw, _ := exec.Command("git", "diff", "--no-index", "-U0", "--no-color", env.RepoDir, work).Output()
	mine := oldSpans(string(raw), strings.TrimPrefix(env.RepoDir, "/"))
	loc := Location{GoldFiles: len(gold)}
	for p := range mine {
		loc.Edited = append(loc.Edited, p)
		if _, ok := gold[p]; !ok {
			loc.ExtraFiles++
		}
	}
	for p, hs := range gold {
		loc.GoldHunks += len(hs)
		ms, ok := mine[p]
		if !ok {
			continue
		}
		loc.FilesHit++
		near := 0
		for _, h := range hs {
			for _, m := range ms {
				if m.lo <= h.hi+locateNear && m.hi >= h.lo-locateNear {
					near++
					break
				}
			}
		}
		loc.HunksNear += near
		if near > 0 {
			loc.FilesNear++
		}
	}
	return loc
}
