package fsx

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// SearchOpts are the fs_search inputs.
type SearchOpts struct {
	Pattern         string
	Path            string
	Glob            string
	Regex           bool
	CaseInsensitive bool
	MaxResults      int
	Context         int
}

// Match is one search hit (or context line when Context is true).
type Match struct {
	Path    string `json:"path"`
	Line    int    `json:"line"`
	Text    string `json:"text"`
	Context bool   `json:"context,omitempty"`
}

// SearchResult is the fs_search output.
type SearchResult struct {
	Pattern   string  `json:"pattern"`
	Path      string  `json:"path"`
	Matches   []Match `json:"matches"`
	Count     int     `json:"count"`
	Truncated bool    `json:"truncated"`
	More      string  `json:"more,omitempty"`
	Engine    string  `json:"engine"`
}

// Search limits.
const (
	DefaultSearchResults = 200
	MaxSearchResults     = 2000
	maxMatchText         = 500
)

// RipgrepBin is the ripgrep binary; tests may clear it to force the fallback.
var RipgrepBin = "rg"

// Search finds pattern under path with ripgrep (respecting .gitignore), or
// a slower built-in walker when rg is missing.
func (f *FS) Search(ctx context.Context, o SearchOpts) (SearchResult, error) {
	if o.Pattern == "" {
		return SearchResult{}, errors.New("pattern is required")
	}
	root := o.Path
	if root == "" {
		root = f.Home
	}
	root, err := f.Resolve(root)
	if err != nil {
		return SearchResult{}, err
	}
	if _, err := os.Stat(root); err != nil {
		return SearchResult{}, fmt.Errorf("stat: %w", err)
	}
	if o.MaxResults <= 0 {
		o.MaxResults = DefaultSearchResults
	}
	o.MaxResults = min(o.MaxResults, MaxSearchResults)
	o.Context = min(max(o.Context, 0), 5)
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	res := SearchResult{Pattern: o.Pattern, Path: root, Matches: []Match{}}
	if bin, err := exec.LookPath(RipgrepBin); err == nil && RipgrepBin != "" {
		res.Engine = "ripgrep"
		err = searchRipgrep(ctx, bin, root, o, &res)
		if err != nil {
			return res, err
		}
	} else {
		res.Engine = "builtin"
		if err := searchBuiltin(ctx, root, o, &res); err != nil {
			return res, err
		}
	}
	if res.Truncated {
		res.More = fmt.Sprintf("stopped after %d matches; narrow path/glob or raise max_results (max %d)", o.MaxResults, MaxSearchResults)
	}
	return res, nil
}

type rgEvent struct {
	Type string `json:"type"`
	Data struct {
		Path       struct{ Text string } `json:"path"`
		Lines      struct{ Text string } `json:"lines"`
		LineNumber int                   `json:"line_number"`
	} `json:"data"`
}

func searchRipgrep(ctx context.Context, bin, root string, o SearchOpts, res *SearchResult) error {
	args := []string{"--json", "--max-columns", strconv.Itoa(maxMatchText), "--max-columns-preview", "--max-filesize", "10M"}
	if !o.Regex {
		args = append(args, "--fixed-strings")
	}
	if o.CaseInsensitive {
		args = append(args, "--ignore-case")
	}
	if o.Glob != "" {
		args = append(args, "--glob", o.Glob)
	}
	if o.Context > 0 {
		args = append(args, "--context", strconv.Itoa(o.Context))
	}
	args = append(args, "--", o.Pattern, root)
	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // G204: fixed binary; args are passed as argv, never through a shell
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("rg pipe: %w", err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start rg: %w", err)
	}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	stopped := false
	for sc.Scan() {
		var ev rgEvent
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		if ev.Type != "match" && ev.Type != "context" {
			continue
		}
		if ev.Type == "match" {
			if res.Count >= o.MaxResults {
				res.Truncated, stopped = true, true
				break
			}
			res.Count++
		}
		res.Matches = append(res.Matches, Match{
			Path: ev.Data.Path.Text, Line: ev.Data.LineNumber,
			Text: strings.TrimRight(ev.Data.Lines.Text, "\r\n"), Context: ev.Type == "context",
		})
	}
	if stopped {
		_ = cmd.Process.Kill()
	}
	err = cmd.Wait()
	if stopped {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return nil // no matches
	}
	if err != nil {
		if ctx.Err() != nil {
			res.Truncated = true
			return nil //nolint:nilerr // a search that hit its deadline returns what it found
		}
		return fmt.Errorf("rg: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func searchBuiltin(ctx context.Context, root string, o SearchOpts, res *SearchResult) error {
	pat := o.Pattern
	if !o.Regex {
		pat = regexp.QuoteMeta(pat)
	}
	if o.CaseInsensitive {
		pat = "(?i)" + pat
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		return fmt.Errorf("bad pattern: %w", err)
	}
	errStop := errors.New("stop")
	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if ctx.Err() != nil {
			res.Truncated = true
			return errStop
		}
		if d.IsDir() {
			if n := d.Name(); p != root && (n == ".git" || n == "node_modules" || n == TrashDirName) {
				return filepath.SkipDir
			}
			return nil
		}
		if o.Glob != "" {
			if ok, _ := filepath.Match(o.Glob, d.Name()); !ok {
				return nil
			}
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() || info.Size() > 10<<20 {
			return nil // skip
		}
		fh, err := os.Open(p) //nolint:gosec // G304: path built by sandboxd or chosen by the calling agent
		if err != nil {
			return nil // skip
		}
		defer func() { _ = fh.Close() }()
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		line := 0
		for sc.Scan() {
			line++
			text := sc.Text()
			if strings.IndexByte(text, 0) >= 0 {
				return nil // binary
			}
			if re.MatchString(text) {
				if res.Count >= o.MaxResults {
					res.Truncated = true
					return errStop
				}
				res.Count++
				if len(text) > maxMatchText {
					text = text[:maxMatchText]
				}
				res.Matches = append(res.Matches, Match{Path: p, Line: line, Text: text})
			}
		}
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, errStop) {
		return fmt.Errorf("walk: %w", walkErr)
	}
	return nil
}
