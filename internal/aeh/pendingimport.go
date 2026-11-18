package aeh

import (
	"errors"
	"fmt"
	"strings"
)

// A pending import is a memory batch saved in a command's local state together with the state
// change it records, before it is imported. A crash between the two never loses or doubles the
// step: the next command of the same session finishes it (stage 4, F3). Discovery sessions and
// adopted areas use it.

// gateBatchFiles is the last check on everything aeh records: no recognized secret, and
// Markdown passes the display policy exactly as it will be written.
func gateBatchFiles(files map[string][]byte) error {
	for _, rel := range sortedKeys(files) {
		if err := checkNoSecrets(rel, files[rel]); err != nil {
			return err
		}
		if strings.HasSuffix(rel, ".md") {
			if err := checkDisplayMarkdown(string(files[rel]), 1<<20); err != nil {
				return fmt.Errorf("%s: %w", rel, err)
			}
		}
	}
	return nil
}

// newPendingImport gates the files and pins the memory HEAD the batch was prepared against.
func newPendingImport(memDir string, files map[string][]byte, subject, log string) (*pendingImport, error) {
	if err := gateBatchFiles(files); err != nil {
		return nil, err
	}
	head, err := git(memDir, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	return &pendingImport{Files: files, ExpectedHead: head, Subject: subject, Log: log}, nil
}

// finishImport imports a saved batch. If the owner committed to memory in between without
// touching the batch's files, the batch moves onto the new HEAD (saved first); otherwise the
// owner decides. The caller clears its pending batch and saves after success.
func finishImport(r Repo, memDir, session string, p *pendingImport, save func() error) error {
	b := memoryBatch{Session: session, ExpectedHead: p.ExpectedHead, Files: p.Files, Remove: p.Remove, Subject: p.Subject, Log: p.Log}
	_, _, err := importMemoryBatch(r, b)
	if errors.Is(err, ErrMemoryChanged) {
		head, herr := git(memDir, "rev-parse", "HEAD")
		if herr != nil {
			return herr
		}
		check := map[string][]byte{}
		for rel, b := range p.Files {
			check[rel] = b
		}
		for _, rel := range p.Remove {
			check[rel] = nil // a removal must not delete a revision committed since
		}
		if touched := batchFilesChanged(memDir, p.ExpectedHead, head, check); touched != "" {
			return fmt.Errorf("memory changed %s since aeh prepared this step; restore it or commit it back, then run the command again", touched)
		}
		p.ExpectedHead = head
		if err := save(); err != nil {
			return err
		}
		b.ExpectedHead = head
		_, _, err = importMemoryBatch(r, b)
	}
	return err
}

// recoverPending prepares memory for a session with a pending batch: dirt that is exactly the
// interrupted import's own is discarded; anything else is reported by memoryClean.
func recoverPending(memDir, session string, p *pendingImport) error {
	if p != nil {
		if err := discardPartialImport(memDir, p, session); err != nil {
			return err
		}
	}
	return memoryClean(memDir)
}
