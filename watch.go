package main

import (
	"bufio"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

const maxHashedFileSize = 20 * 1024 * 1024

func digest(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if stat.IsDir() {
		return nil, nil
	}
	h := sha256.New()
	br := bufio.NewReader(f)
	if _, err := io.Copy(h, io.LimitReader(br, maxHashedFileSize)); err != nil {
		return nil, err
	}
	if b, _ := br.Peek(1); len(b) > 0 {
		// too big
		return nil, nil
	}
	return h.Sum(nil), nil
}

// watch watches the input paths. The returned Watcher should only be used in
// tests.
func watch(inputPaths, ignoredPaths []string, cmdCh chan<- event) (map[string][]byte, error) {
	// Creates an Ignorer that just ignores file paths the user
	// specifically asked to be ignored.
	ui, err := createUserIgnorer(ignoredPaths)
	if err != nil {
		return nil, err
	}

	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("unable to create watcher: %s", err)
	}

	// Watch user-specified paths and create a set of them for walking
	// later. Paths that are both asked to be watched and ignored by
	// the user are ignored.
	userPaths := make(map[string][]byte)
	includedHiddenFiles := make(map[string]bool)
	for _, path := range inputPaths {
		fullPath, err := filepath.Abs(path)
		if err != nil {
			w.Close()
			return nil, errors.New("unable to get current working directory while working with user-watched paths")
		}
		_, found := userPaths[fullPath]
		if found || ui.IsIgnored(path) {
			continue
		}
		err = w.Add(fullPath)
		if err != nil {
			w.Close()
			return nil, fmt.Errorf("unable to watch '%s': %s", path, err)
		}
		d, _ := digest(fullPath)
		userPaths[fullPath] = d
	}

	// Create some useful sets from the user-specified paths to be
	// used in smartIgnorer (and, therefore, listenForEvents). One is
	// the set of hidden paths that the user does not want ignored to
	// be used in smartIgnorer. We create this smaller map because the
	// amount of paths the user asked to watch may be large.
	//
	// We also create the sets renameDirs and renameChildren to better
	// handle files that are renamed away and back from the paths the
	// user wanted watched. To be more concrete, folks might want to
	// watch only the normal file "foobar" but their tooling moves
	// foobar away and then back (like vim does on save). This will
	// cause the watch to fire on the first move but then never again,
	// even when its returned. So, we have to track the parent
	// directory of foobar in order to capture when foobar shows up in
	// its parent directory again but we don't want to send all events
	// in that parent directory.
	renameDirs := make(map[string]bool)
	renameChildren := make(map[string]bool)
	for fullPath, _ := range userPaths {
		baseName := filepath.Base(fullPath)
		if strings.HasPrefix(baseName, ".") {
			includedHiddenFiles[fullPath] = true
		}

		dirPath := filepath.Dir(fullPath)
		_, foundDir := userPaths[dirPath]
		if !foundDir && dirPath != "" {
			if !renameDirs[dirPath] {
				err = w.Add(dirPath)
				if err != nil {
					w.Close()
					return nil, fmt.Errorf("unable to watch rename-watched-only dir '%s': %s", fullPath, err)
				}
			}
			renameDirs[dirPath] = true
			renameChildren[fullPath] = true
		}
	}
	ig := &smartIgnorer{
		includedHiddenFiles: includedHiddenFiles,
		ui:                  ui,
		renameDirs:          renameDirs,
		renameChildren:      renameChildren,
	}

	go listenForEvents(w, cmdCh, ig)
	return userPaths, nil
}

type event struct {
	time.Time
	Event fsnotify.Event
}

func listenForEvents(w *fsnotify.Watcher, cmdCh chan<- event, ignorer Ignorer) {
	for {
		select {
		case ev, ok := <-w.Events:
			if !ok {
				return
			}
			if ignorer.IsIgnored(ev.Name) {
				continue
			}
			if *verbose {
				log.Printf("unignored file change: %s", ev)
			}
			cmdCh <- event{
				Time:  time.Now(),
				Event: ev,
			}
		case err, ok := <-w.Errors:
			if !ok {
				close(cmdCh)
				return
			}
			// w.Close causes this.
			if err == nil {
				close(cmdCh)
				return
			}
			log.Println("watch error:", err)
		}
	}
}

func createUserIgnorer(ignoredPaths []string) (*userIgnorer, error) {
	ignored := make(map[string]bool)
	ignoredDirs := make([]string, 0)
	for _, in := range ignoredPaths {
		in = strings.TrimSpace(in)
		if len(in) == 0 {
			continue
		}
		path, err := filepath.Abs(in)
		if err != nil {
			return nil, errors.New("unable to get current working dir while working with ignored paths")
		}
		ignored[path] = true
		dirPath := path
		if !strings.HasSuffix(path, "/") {
			dirPath += "/"
		}
		ignoredDirs = append(ignoredDirs, dirPath)
	}
	return &userIgnorer{ignored, ignoredDirs}, nil
}
