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
	records, err := c.b.store.ListAHPRecords(c.ctx, c.principal.UserKey, ahpRecordFile)
	if err != nil {
		return nil
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
	return files
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
	for full := range c.files() {
		if _, ok := changed[full]; !ok {
			c.removeFile(full)
		}
	}
	for full, change := range changed {
		c.setFile(full, change.Before, change.After, false)
	}
	c.emitCounts()
}
