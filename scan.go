//go:build linux

package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Script detection limits
const (
	scriptExtension = ".sh"
	shebangLimit    = 256
)

// Shells whose shebang marks a file without extension as a script
var shells = []string{"sh", "bash", "dash", "zsh", "ksh", "fish"}

// Shell script found in the directory
type Script struct {
	Name    string   `json:"name"`
	Path    string   `json:"path"`
	Target  string   `json:"target"`
	Shebang []string `json:"-"`
}

// Scripts in directory, sorted by name and path; recursive skips hidden subdirectories
func findScripts(directory string, recursive bool) ([]Script, error) {
	var scripts []Script
	err := filepath.WalkDir(directory, func(path string, entry fs.DirEntry, err error) error {
		if err != nil && path == directory {
			return err
		}
		if err != nil {
			// Unreadable subdirectories are skipped
			if entry != nil && entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			if path != directory && (!recursive || strings.HasPrefix(entry.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		script, ok, err := inspectScript(path)
		if ok {
			scripts = append(scripts, script)
		}
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %v", directory, err)
	}
	slices.SortFunc(scripts, func(a, b Script) int {
		return strings.Compare(a.Name+"\x00"+a.Path, b.Name+"\x00"+b.Path)
	})
	return scripts, nil
}

// Script data when path is a .sh file or a shell file without extension; symlinks must end at a regular file
func inspectScript(path string) (Script, bool, error) {
	name := filepath.Base(path)
	extension := filepath.Ext(name)
	if extension != scriptExtension && extension != "" {
		return Script{}, false, nil
	}
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		// Broken symlinks are no scripts
		return Script{}, false, nil
	}
	info, err := os.Stat(target)
	if err != nil || !info.Mode().IsRegular() {
		return Script{}, false, nil
	}
	shebang, err := readShebang(target)
	if err != nil {
		// Unreadable files are no scripts
		return Script{}, false, nil
	}
	if extension == "" && !isShell(shebang) {
		return Script{}, false, nil
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return Script{}, false, err
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return Script{}, false, err
	}
	return Script{strings.TrimSuffix(name, scriptExtension), absolute, target, shebang}, true, nil
}

// Words of the first line after #!, read from at most the first 256 bytes
func readShebang(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	buffer := make([]byte, shebangLimit)
	count, err := io.ReadFull(file, buffer)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, err
	}
	line, _, _ := strings.Cut(string(buffer[:count]), "\n")
	words, ok := strings.CutPrefix(line, "#!")
	if !ok {
		return nil, nil
	}
	return strings.Fields(words), nil
}

// Shebang runs a known shell, directly or through env
func isShell(shebang []string) bool {
	command := shebangCommand(shebang)
	return command != "" && slices.Contains(shells, filepath.Base(command))
}

// Program the shebang starts: the first word, or the first non-option word after env
func shebangCommand(shebang []string) string {
	if len(shebang) == 0 {
		return ""
	}
	if filepath.Base(shebang[0]) != "env" {
		return shebang[0]
	}
	arguments := shebang[1:]
	for index := 0; index < len(arguments); index++ {
		word := arguments[index]
		switch {
		case slices.Contains([]string{"-u", "--unset", "-C", "--chdir"}, word):
			index++
		case !strings.HasPrefix(word, "-") && !strings.Contains(word, "="):
			return word
		}
	}
	return ""
}
