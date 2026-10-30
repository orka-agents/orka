/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/microsoft/agent-host-protocol/clients/go/ahptypes"

	"github.com/orka-agents/orka/internal/ahphost"
	"github.com/orka-agents/orka/internal/store"
)

// ahpChanges keeps one session's changeset: a file record per changed file,
// with the contents its before and after sides name, and the actions that
// show them.
type ahpChanges struct {
	b         *ahpBackend
	ctx       context.Context
	principal ahphost.Principal
	session   ahpSession
	emit      ahphost.Emitter
	// released holds the contents files stopped naming during a change.
	released map[string]bool
}

// change makes a batch of changes, then deletes the stored contents the batch
// left no file naming. The session's files share contents, so batches take
// turns: one cannot delete a content another is about to name.
func (c *ahpChanges) change(fn func()) {
	unlock := c.b.lockChanges(c.principal.UserKey)
	defer unlock()
	fn()
	c.dropReleased()
}

// release notes contents a file may no longer name.
func (c *ahpChanges) release(nonces ...string) {
	for _, nonce := range nonces {
		if nonce == "" {
			continue
		}
		if c.released == nil {
			c.released = map[string]bool{}
		}
		c.released[nonce] = true
	}
}

// dropReleased deletes the released contents no file of the session names.
// When the files cannot be listed, it keeps them.
func (c *ahpChanges) dropReleased() {
	released := c.released
	c.released = nil
	if len(released) == 0 {
		return
	}
	files, err := c.listFiles()
	if err != nil {
		return
	}
	for _, file := range files {
		delete(released, file.Before)
		delete(released, file.After)
	}
	for nonce := range released {
		_ = c.b.store.DeleteAHPRecord(c.ctx, c.principal.UserKey, ahpRecordContent, c.hash()+"/"+nonce)
	}
}

func (c *ahpChanges) hash() string { return ahpHash(c.principal.UserKey, c.session.Session) }

func (c *ahpChanges) contentURI(nonce string) string {
	return ahpContentScheme + c.hash()[:24] + "/" + nonce
}

// putContent stores a side's content and returns its nonce.
func (c *ahpChanges) putContent(text string) (string, bool) {
	nonce := "sha256:" + ahpHash(text)
	err := c.b.store.PutAHPRecord(c.ctx, store.AHPRecord{
		UserKey: c.principal.UserKey, Kind: ahpRecordContent, Key: c.hash() + "/" + nonce, Value: []byte(text), UpdatedAt: c.b.now().UTC(),
	})
	return nonce, err == nil
}

func (c *ahpChanges) text(nonce string) string {
	if nonce == "" {
		return ""
	}
	if record, err := c.b.store.GetAHPRecord(c.ctx, c.principal.UserKey, ahpRecordContent, c.hash()+"/"+nonce); err == nil {
		return string(record.Value)
	}
	return ""
}

// setFile records a changed file and shows it. A nil before is a new file
// and a nil after a deleted one. keepBefore keeps the before side a file
// already has, as reported edits accumulate on the first version.
func (c *ahpChanges) setFile(full string, before, after *string, keepBefore bool) {
	var file ahpFileContent
	key := c.hash() + "/" + full
	err := c.b.getRecord(c.ctx, c.principal.UserKey, ahpRecordFile, key, &file)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return
	}
	known := err == nil
	// Whatever this leaves unnamed, including a content stored for a write
	// that then fails, goes when the change ends.
	previous := file
	defer func() { c.release(previous.Before, previous.After, file.Before, file.After) }()
	if !known || !keepBefore {
		file.Before = ""
		if before != nil {
			nonce, ok := c.putContent(*before)
			if !ok {
				return
			}
			file.Before = nonce
		}
	}
	file.Path, file.After, file.Deleted = full, "", after == nil
	if after != nil {
		nonce, ok := c.putContent(*after)
		if !ok {
			return
		}
		file.After = nonce
	}
	// A file edited back to how it began, or created and deleted again, no
	// longer differs: nothing else would take it off the changeset.
	if file.Before == file.After {
		c.removeFile(full)
		return
	}
	if c.b.putRecord(c.ctx, c.principal.UserKey, ahpRecordFile, key, file) != nil {
		return
	}
	c.emitFile(file)
}

// removeFile drops a file that no longer differs.
func (c *ahpChanges) removeFile(full string) {
	if c.b.store.DeleteAHPRecord(c.ctx, c.principal.UserKey, ahpRecordFile, c.hash()+"/"+full) != nil {
		return
	}
	_ = c.emit.Emit(c.ctx, c.session.Changeset, ahptypes.StateAction{Value: &ahptypes.ChangesetFileRemovedAction{
		Type: ahptypes.ActionTypeChangesetFileRemoved, FileId: ahpHash(full)[:16],
	}})
}

func (c *ahpChanges) emitFile(file ahpFileContent) {
	uri := ahpDirectoryURI(file.Path)
	var edit ahptypes.FileEdit
	if file.After != "" {
		after := file.After
		edit.After = &ahptypes.FileEditSide{Uri: uri, Content: ahptypes.ContentRef{Uri: c.contentURI(after), Nonce: &after}}
	}
	if file.Before != "" {
		before := file.Before
		edit.Before = &ahptypes.FileEditSide{Uri: uri, Content: ahptypes.ContentRef{Uri: c.contentURI(before), Nonce: &before}}
	}
	added, removed := ahpLineChanges(c.text(file.Before), c.text(file.After))
	edit.Diff = &ahptypes.FileEditDiffStats{Added: &added, Removed: &removed}
	_ = c.emit.Emit(c.ctx, c.session.Changeset, ahptypes.StateAction{Value: &ahptypes.ChangesetFileSetAction{
		Type: ahptypes.ActionTypeChangesetFileSet, File: ahptypes.ChangesetFile{Id: ahpHash(file.Path)[:16], Edit: edit},
	}})
}

// files returns the session's changed files by path.
func (c *ahpChanges) files() map[string]ahpFileContent {
	files, _ := c.listFiles()
	return files
}

func (c *ahpChanges) listFiles() (map[string]ahpFileContent, error) {
	records, err := c.b.store.ListAHPRecords(c.ctx, c.principal.UserKey, ahpRecordFile)
	if err != nil {
		return nil, err
	}
	files := map[string]ahpFileContent{}
	for _, record := range records {
		if !strings.HasPrefix(record.Key, c.hash()+"/") {
			continue
		}
		var file ahpFileContent
		if json.Unmarshal(record.Value, &file) == nil {
			files[file.Path] = file
		}
	}
	return files, nil
}

// emitCounts updates the chat's change counts from every changed file.
func (c *ahpChanges) emitCounts() {
	var additions, deletions, count int64
	for _, file := range c.files() {
		added, removed := ahpLineChanges(c.text(file.Before), c.text(file.After))
		additions, deletions, count = additions+added, deletions+removed, count+1
	}
	_ = c.emit.Emit(c.ctx, c.session.Session, ahptypes.StateAction{Value: &ahptypes.SessionChatUpdatedAction{
		Type: ahptypes.ActionTypeSessionChatUpdated, Chat: c.session.Chat,
		Changes: ahptypes.PartialChatSummary{Changes: &ahptypes.ChangesSummary{Additions: &additions, Deletions: &deletions, Files: &count}},
	}})
}

// ahpGitChange is one file's sides in a branch's diff; nil is absent.
type ahpGitChange struct {
	Before, After *string
}

// sync makes the changeset exactly the given files.
func (c *ahpChanges) sync(changed map[string]ahpGitChange) {
	c.change(func() {
		for full, file := range c.files() {
			if _, ok := changed[full]; !ok {
				c.removeFile(full)
				c.release(file.Before, file.After)
			}
		}
		for full, change := range changed {
			c.setFile(full, change.Before, change.After, false)
		}
	})
	c.emitCounts()
}
