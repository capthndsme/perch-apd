// Package uci reads and edits OpenWrt UCI configuration files in place.
//
// Only what the agent needs: named sections, options and lists. Edits keep
// every other line (comments, other sections, unknown syntax) byte for byte,
// so a file the admin annotated stays annotated after the agent stores its
// credentials in it.
package uci

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type lineKind int

const (
	kindOther lineKind = iota
	kindSection
	kindOption
	kindList
)

type line struct {
	raw     string
	kind    lineKind
	section int // index into File.sections, -1 before the first section
	key     string
	value   string
}

type section struct {
	typ  string
	name string
}

// File is a parsed UCI file that can be edited and written back.
type File struct {
	lines    []line
	sections []section
}

// Parse parses UCI syntax. It never fails on unknown lines; they are kept.
func Parse(data []byte) (*File, error) {
	f := &File{}
	current := -1
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	text = strings.TrimSuffix(text, "\n")
	if text == "" {
		return f, nil
	}
	for _, raw := range strings.Split(text, "\n") {
		l := line{raw: raw, kind: kindOther, section: current}
		words, err := tokenize(raw)
		if err != nil || len(words) == 0 {
			f.lines = append(f.lines, l)
			continue
		}
		switch words[0] {
		case "config":
			if len(words) < 2 {
				break
			}
			s := section{typ: words[1]}
			if len(words) > 2 {
				s.name = words[2]
			}
			f.sections = append(f.sections, s)
			current = len(f.sections) - 1
			l.kind = kindSection
			l.section = current
		case "option", "list":
			if current < 0 || len(words) < 2 {
				break
			}
			l.key = words[1]
			if len(words) > 2 {
				l.value = words[2]
			}
			if words[0] == "option" {
				l.kind = kindOption
			} else {
				l.kind = kindList
			}
		}
		f.lines = append(f.lines, l)
	}
	return f, nil
}

// Load reads and parses a file. A missing file is an empty File.
func Load(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &File{}, nil
	}
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

func (f *File) sectionIndex(name string) int {
	for i, s := range f.sections {
		if s.name == name {
			return i
		}
	}
	return -1
}

// HasSection reports whether a section with this name exists.
func (f *File) HasSection(name string) bool { return f.sectionIndex(name) >= 0 }

// Get returns an option of a named section.
func (f *File) Get(sectionName, option string) (string, bool) {
	idx := f.sectionIndex(sectionName)
	if idx < 0 {
		return "", false
	}
	for _, l := range f.lines {
		if l.kind == kindOption && l.section == idx && l.key == option {
			return l.value, true
		}
	}
	return "", false
}

// GetList returns the values of a list option (in file order).
func (f *File) GetList(sectionName, option string) []string {
	idx := f.sectionIndex(sectionName)
	var out []string
	for _, l := range f.lines {
		if l.kind == kindList && l.section == idx && l.key == option {
			out = append(out, l.value)
		}
	}
	return out
}

// EnsureSection appends `config <typ> '<name>'` when no section has that name.
func (f *File) EnsureSection(typ, name string) {
	if f.sectionIndex(name) >= 0 {
		return
	}
	if len(f.lines) > 0 && strings.TrimSpace(f.lines[len(f.lines)-1].raw) != "" {
		f.lines = append(f.lines, line{raw: "", kind: kindOther, section: len(f.sections) - 1})
	}
	f.sections = append(f.sections, section{typ: typ, name: name})
	idx := len(f.sections) - 1
	f.lines = append(f.lines, line{
		raw:     fmt.Sprintf("config %s %s", typ, Quote(name)),
		kind:    kindSection,
		section: idx,
	})
}

// Set replaces the option's line, or inserts one after the section's last
// option (or its header). The section must exist.
func (f *File) Set(sectionName, option, value string) error {
	if !validName(option) {
		return fmt.Errorf("uci: invalid option name %q", option)
	}
	idx := f.sectionIndex(sectionName)
	if idx < 0 {
		return fmt.Errorf("uci: no section %q", sectionName)
	}
	raw := fmt.Sprintf("\toption %s %s", option, Quote(value))
	insertAt := -1
	for i, l := range f.lines {
		if l.section != idx {
			continue
		}
		switch l.kind {
		case kindOption:
			if l.key == option {
				f.lines[i] = line{raw: raw, kind: kindOption, section: idx, key: option, value: value}
				return nil
			}
			insertAt = i + 1
		case kindList:
			insertAt = i + 1
		case kindSection:
			if insertAt < 0 {
				insertAt = i + 1
			}
		}
	}
	nl := line{raw: raw, kind: kindOption, section: idx, key: option, value: value}
	f.lines = append(f.lines[:insertAt], append([]line{nl}, f.lines[insertAt:]...)...)
	return nil
}

// Delete removes an option line. Missing options are not an error.
func (f *File) Delete(sectionName, option string) {
	idx := f.sectionIndex(sectionName)
	if idx < 0 {
		return
	}
	out := f.lines[:0]
	for _, l := range f.lines {
		if l.kind == kindOption && l.section == idx && l.key == option {
			continue
		}
		out = append(out, l)
	}
	f.lines = out
}

// Bytes renders the file.
func (f *File) Bytes() []byte {
	var b bytes.Buffer
	for _, l := range f.lines {
		b.WriteString(l.raw)
		b.WriteByte('\n')
	}
	return b.Bytes()
}

// WriteFile writes atomically (temp file + rename in the same directory),
// keeping the existing file's mode or using perm for a new one.
func (f *File) WriteFile(path string, perm os.FileMode) error {
	if st, err := os.Stat(path); err == nil {
		perm = st.Mode().Perm()
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(f.Bytes()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// Quote renders a value the way `uci export` does: in single quotes, an
// embedded single quote escaped the shell way (close quote, backslash + quote,
// reopen quote).
func Quote(v string) string {
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}

func validName(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if !(c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// tokenize splits one line into words with UCI/shell quoting: 'single'
// (literal), "double" (backslash escapes), bare words, adjacent pieces
// concatenated (a quoted piece, a backslash-escaped quote, another quoted
// piece form one word), '#' starting a comment outside quotes.
func tokenize(s string) ([]string, error) {
	var words []string
	var cur strings.Builder
	inWord := false
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == '#' && !inWord:
			return words, nil
		case c == ' ' || c == '\t':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
			i++
		case c == '\'':
			end := strings.IndexByte(s[i+1:], '\'')
			if end < 0 {
				return nil, errors.New("unterminated single quote")
			}
			cur.WriteString(s[i+1 : i+1+end])
			inWord = true
			i += end + 2
		case c == '"':
			i++
			closed := false
			for i < len(s) {
				if s[i] == '\\' && i+1 < len(s) {
					cur.WriteByte(s[i+1])
					i += 2
					continue
				}
				if s[i] == '"' {
					closed = true
					i++
					break
				}
				cur.WriteByte(s[i])
				i++
			}
			if !closed {
				return nil, errors.New("unterminated double quote")
			}
			inWord = true
		case c == '\\' && i+1 < len(s):
			cur.WriteByte(s[i+1])
			inWord = true
			i += 2
		default:
			cur.WriteByte(c)
			inWord = true
			i++
		}
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words, nil
}
