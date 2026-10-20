package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/hiveryn/daemon/internal/actionfs"
	"github.com/hiveryn/daemon/internal/domain"
)

func runActionInit(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		say(stdout, "usage: hiverynd action init [path]\nCreate a minimal Action without overwriting files; path defaults to the current directory.\n")
		return exitOK
	}
	if len(args) > 1 || (len(args) == 1 && strings.HasPrefix(args[0], "-")) {
		say(stderr, "usage: hiverynd action init [path]\n")
		return exitUsageErr
	}
	target := "."
	if len(args) == 1 {
		target = args[0]
	}
	dir, err := filepath.Abs(target)
	if err == nil {
		err = initAction(dir)
	}
	if err != nil {
		say(stderr, "hiverynd action init: %v\n", err)
		return exitUsageErr
	}
	say(stdout, "Created Action %s in %s. Not registered, launched or committed.\n", filepath.Base(dir), dir)
	return exitOK
}

func initAction(dir string) error {
	name := filepath.Base(dir)
	if !domain.ValidActionName(name) {
		return fmt.Errorf("invalid action name %q: choose a directory name starting with a lowercase letter or digit, using only lowercase letters, digits, '.', '_' or '-' (at most 64 characters)", name)
	}
	for _, file := range []string{actionfs.DefinitionFileName, actionfs.KickoffFileName} {
		path := filepath.Join(dir, file)
		if _, err := os.Lstat(path); err == nil {
			return fmt.Errorf("refusing to overwrite %s", path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	gitPath := filepath.Join(dir, ".git")
	if _, err := os.Lstat(gitPath); errors.Is(err, os.ErrNotExist) {
		if out, err := exec.Command("git", "init", "--", dir).CombinedOutput(); err != nil {
			return fmt.Errorf("initialize Git: %w: %s", err, out)
		}
	} else if err != nil {
		return err
	}
	// Check existing repositories without reinitializing or changing their state.
	if out, err := exec.Command("git", "-C", dir, "rev-parse", "--git-dir").CombinedOutput(); err != nil {
		return fmt.Errorf("inspect Git repository: %w: %s", err, out)
	}
	manifest := fmt.Sprintf("name: %q\ndescription: Write a short greeting for the person named in the request. Supply their name.\nartifacts: greeting.txt containing the requested greeting.\nsuggestions:\n  - Write a short greeting for Alex.\n", name)
	kickoff := "Write the requested greeting as greeting.txt in the output directory. Verify the file contains the greeting, then conclude with that directory as delivery_dir.\n\nRequest:\n{{prompt}}\n\nOutput directory:\n{{output_dir}}\n"
	for _, file := range []struct{ name, body string }{{actionfs.DefinitionFileName, manifest}, {actionfs.KickoffFileName, kickoff}} {
		path := filepath.Join(dir, file.name)
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return fmt.Errorf("create %s: %w", path, err)
		}
		_, writeErr := io.WriteString(f, file.body)
		closeErr := f.Close()
		if err := errors.Join(writeErr, closeErr); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
	}
	def, err := actionfs.Inspect(dir)
	if err != nil {
		return err
	}
	if !def.Valid {
		return actionfs.InvalidError(def)
	}
	return nil
}
