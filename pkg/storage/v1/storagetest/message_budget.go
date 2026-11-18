/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package storagetest

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

// runMessageBudget checks the byte limits on inter-agent messages: a message's
// content is capped when it is sent, and a batch is cut short by its content
// bytes before any message in it is marked read, so a consumed batch always
// fits one response.
func runMessageBudget(t *testing.T, newProvider StateFactory) {
	require.LessOrEqual(t, storagev1.MaxMessageContentBytes, storagev1.MaxMessageBatchBytes,
		"any stored message fits a batch of its own")

	t.Run("content is capped in bytes when a message is sent", func(t *testing.T) {
		provider := newProvider(t)
		ctx := t.Context()
		// A two-byte rune makes the provider count bytes, not characters.
		atLimit := strings.Repeat("é", storagev1.MaxMessageContentBytes/2)
		require.Len(t, atLimit, storagev1.MaxMessageContentBytes)
		sent := inboxMessage(atLimit)
		require.NoError(t, provider.SendMessage(ctx, sent))
		require.Positive(t, sent.ID)

		over := atLimit + "x"
		err := provider.SendMessage(ctx, inboxMessage(over))
		require.ErrorIs(t, err, storagev1.ErrTooLarge)
		var tooLarge storagev1.TooLargeError
		require.True(t, errors.As(err, &tooLarge), "the refusal is a TooLargeError: %v", err)
		require.Equal(t, int64(len(over)), tooLarge.Size)
		require.Equal(t, int64(storagev1.MaxMessageContentBytes), tooLarge.Limit)

		read, err := provider.ConsumeMessages(ctx, inboxRequest(0))
		require.NoError(t, err)
		require.Len(t, read, 1, "the refused message was not stored")
		require.Equal(t, sent.ID, read[0].ID)
		require.Equal(t, atLimit, read[0].Content, "content at the limit is returned whole")
		left, err := provider.ListUnreadMessages(ctx, "tenant", "reader", "parent", 0)
		require.NoError(t, err)
		require.Empty(t, left)
	})

	t.Run("a batch stops before its content passes the byte budget", func(t *testing.T) {
		provider := newProvider(t)
		ctx := t.Context()
		perBatch := storagev1.MaxMessageBatchBytes / storagev1.MaxMessageContentBytes
		ids := make([]int64, perBatch+1)
		for i := range ids {
			message := inboxMessage(budgetContent(i, storagev1.MaxMessageContentBytes))
			require.NoError(t, provider.SendMessage(ctx, message))
			ids[i] = message.ID
		}

		listed, err := provider.ListUnreadMessages(ctx, "tenant", "reader", "parent", 0)
		require.NoError(t, err)
		require.Equal(t, ids[:perBatch], budgetMessageIDs(listed), "the oldest messages that fit the budget, by ID")
		listedAgain, err := provider.ListUnreadMessages(ctx, "tenant", "reader", "parent", 0)
		require.NoError(t, err)
		require.Equal(t, ids[:perBatch], budgetMessageIDs(listedAgain), "listing changes nothing")

		first, err := provider.ConsumeMessages(ctx, inboxRequest(0))
		require.NoError(t, err)
		require.Equal(t, ids[:perBatch], budgetMessageIDs(first))
		requireBatchWithinBudget(t, first)
		unread, err := provider.ListUnreadMessages(ctx, "tenant", "reader", "parent", 0)
		require.NoError(t, err)
		require.Equal(t, ids[perBatch:], budgetMessageIDs(unread), "a message left out of the batch is not marked read")

		second, err := provider.ConsumeMessages(ctx, inboxRequest(0))
		require.NoError(t, err)
		require.Equal(t, ids[perBatch:], budgetMessageIDs(second))
		require.Equal(t, budgetContent(perBatch, storagev1.MaxMessageContentBytes), second[0].Content)
	})

	t.Run("a batch of exactly the byte budget is returned whole", func(t *testing.T) {
		for _, overBy := range []int{0, 1} {
			t.Run(fmt.Sprintf("over by %d", overBy), func(t *testing.T) {
				provider := newProvider(t)
				ctx := t.Context()
				// Full messages, then a short one that brings the total to the
				// budget, then a one-byte message, overBy bytes longer.
				sizes := []int{}
				remaining := storagev1.MaxMessageBatchBytes
				for remaining > storagev1.MaxMessageContentBytes {
					sizes = append(sizes, storagev1.MaxMessageContentBytes)
					remaining -= storagev1.MaxMessageContentBytes
				}
				sizes = append(sizes, remaining-1, 1+overBy)
				ids := make([]int64, len(sizes))
				for i, size := range sizes {
					message := inboxMessage(budgetContent(i, size))
					require.NoError(t, provider.SendMessage(ctx, message))
					ids[i] = message.ID
				}
				want := ids
				if overBy > 0 {
					want = ids[:len(ids)-1]
				}
				listed, err := provider.ListUnreadMessages(ctx, "tenant", "reader", "parent", 0)
				require.NoError(t, err)
				require.Equal(t, want, budgetMessageIDs(listed))
				consumed, err := provider.ConsumeMessages(ctx, inboxRequest(0))
				require.NoError(t, err)
				require.Equal(t, want, budgetMessageIDs(consumed))
				requireBatchWithinBudget(t, consumed)
				rest, err := provider.ConsumeMessages(ctx, inboxRequest(0))
				require.NoError(t, err)
				require.Equal(t, ids[len(want):], budgetMessageIDs(rest), "the message past the budget comes in the next batch")
			})
		}
	})

	t.Run("a replayed budget-limited consume marks nothing more", func(t *testing.T) {
		provider := newProvider(t)
		advanceEpoch(t, provider, suiteFence)
		dataset := datasetID(t, provider)
		ctx := t.Context()
		perBatch := storagev1.MaxMessageBatchBytes / storagev1.MaxMessageContentBytes
		ids := make([]int64, perBatch+1)
		for i := range ids {
			message := inboxMessage(budgetContent(i, storagev1.MaxMessageContentBytes))
			require.NoError(t, provider.SendMessage(ctx, message))
			ids[i] = message.ID
		}

		consumeCtx := storagev1.WithWriteEnvelope(ctx, NewWriteEnvelope(t, dataset, suiteFence, "ConsumeMessages", inboxRequest(0)))
		first, err := provider.ConsumeMessages(consumeCtx, inboxRequest(0))
		require.NoError(t, err)
		require.Equal(t, ids[:perBatch], budgetMessageIDs(first))
		replay, err := provider.ConsumeMessages(consumeCtx, inboxRequest(0))
		require.NoError(t, err)
		require.Equal(t, budgetMessageIDs(first), budgetMessageIDs(replay), "the replay returns the same batch")
		require.Equal(t, first[0].Content, replay[0].Content)

		unread, err := provider.ListUnreadMessages(ctx, "tenant", "reader", "parent", 0)
		require.NoError(t, err)
		require.Equal(t, ids[perBatch:], budgetMessageIDs(unread), "the replay marked nothing more")
	})
}

// budgetContent returns size bytes of content that differ from message to
// message by index.
func budgetContent(index, size int) string {
	return strings.Repeat(string(rune('a'+index%26)), size)
}

// budgetMessageIDs returns the IDs of messages, so a failed comparison does not
// print megabytes of content.
func budgetMessageIDs(messages []storagev1.Message) []int64 {
	ids := make([]int64, len(messages))
	for i, message := range messages {
		ids[i] = message.ID
	}
	return ids
}

func requireBatchWithinBudget(t *testing.T, messages []storagev1.Message) {
	t.Helper()
	total := 0
	for _, message := range messages {
		total += len(message.Content)
	}
	require.LessOrEqual(t, total, storagev1.MaxMessageBatchBytes, "a batch's content stays within the byte budget")
}
