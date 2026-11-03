// Package pathresolve implements non-strict realpath resolution for configured paths.
package pathresolve

import (
	"os"
	"path/filepath"
	"strings"
)

// Real resolves existing symlinks before processing .. and permits missing suffixes.
// filepath.Abs/Join must not clean the input first: link/.. refers to the link target's parent.
func Real(path string) (string, error) {
	if !filepath.IsAbs(path) {
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		path = cwd + "/" + path
	}
	candidate := strings.TrimRight(path, "/")
	if candidate == "" {
		candidate = "/"
	}
	var missing []string
	for {
		resolved, err := filepath.EvalSymlinks(candidate)
		if err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		index := strings.LastIndexByte(candidate, '/')
		if index < 0 {
			return "", err
		}
		missing = append(missing, candidate[index+1:])
		candidate = strings.TrimRight(candidate[:index], "/")
		if candidate == "" {
			candidate = "/"
		}
	}
}
