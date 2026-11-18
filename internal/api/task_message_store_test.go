package api

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/tools"
)

func TestBrokeredMessagingRequiresAuthenticatedAncestry(t *testing.T) {
	for _, test := range []string{"valid", "ownerless caller", "wrong parent UID", "replaced parent", "ownerless recipient", "wrong recipient parent UID", "protection disabled", "stale caller", "forged sender", "missing store", "transactions unavailable", "prompt guard unavailable"} {
		t.Run(test, func(t *testing.T) {
			task, kube, data := setupBrokeredTranscriptSearch(t)
			require.NoError(t, data.SendMessage(t.Context(), &store.Message{
				Namespace: task.Namespace, FromTask: "peer", ToTask: "*", ParentTask: "root", Content: "original broadcast",
			}))
			var backing store.MessageStore = data
			if test == "transactions unavailable" {
				backing = struct{ store.MessageStore }{data}
			}
			guard := allowBrokeredTaskData
			if test == "prompt guard unavailable" {
				guard = nil
			}
			toolContext := &tools.ToolContext{
				Brokered: true, Client: kube, Namespace: task.Namespace, TaskID: task.Name, TaskUID: string(task.UID), ParentTaskID: "root",
				MessageStore: NewTaskMessageStore(kube, backing, client.ObjectKeyFromObject(task), string(task.UID), test != "protection disabled", guard),
			}
			ctx := tools.WithToolContext(t.Context(), toolContext)
			switch test {
			case "ownerless caller":
				task.OwnerReferences = nil
				require.NoError(t, kube.Update(t.Context(), task))
			case "wrong parent UID":
				task.OwnerReferences[0].UID = "foreign-parent-uid"
				require.NoError(t, kube.Update(t.Context(), task))
			case "replaced parent":
				parent := &corev1alpha1.Task{}
				require.NoError(t, kube.Get(t.Context(), client.ObjectKey{Namespace: task.Namespace, Name: "root"}, parent))
				require.NoError(t, kube.Delete(t.Context(), parent))
				parent.UID, parent.ResourceVersion = "replacement-parent-uid", ""
				require.NoError(t, kube.Create(t.Context(), parent))
			case "ownerless recipient", "wrong recipient parent UID":
				peer := &corev1alpha1.Task{}
				require.NoError(t, kube.Get(t.Context(), client.ObjectKey{Namespace: task.Namespace, Name: "peer"}, peer))
				if test == "ownerless recipient" {
					peer.OwnerReferences = nil
				} else {
					peer.OwnerReferences[0].UID = "foreign-parent-uid"
				}
				require.NoError(t, kube.Update(t.Context(), peer))
			case "stale caller":
				require.NoError(t, kube.Delete(t.Context(), task))
				task.UID, task.ResourceVersion = "replacement-task-uid", ""
				require.NoError(t, kube.Create(t.Context(), task))
			case "forged sender":
				toolContext.TaskID = "another-task"
			case "missing store":
				toolContext.MessageStore = nil
			}
			allowInbox := test == "valid" || test == "ownerless recipient" || test == "wrong recipient parent UID"
			for _, target := range []string{"peer", "*"} {
				args, err := json.Marshal(tools.SendMessageArgs{ToTask: target, Content: "new message"})
				require.NoError(t, err)
				result, err := tools.NewSendMessageTool().Execute(ctx, args)
				if test == "valid" || (target == "*" && allowInbox) {
					require.NoError(t, err)
					require.Contains(t, result, "Message sent")
				} else {
					require.Error(t, err)
					require.Empty(t, result)
				}
			}
			result, err := tools.NewCheckMessagesTool().Execute(ctx, json.RawMessage(`{"mark_read":true}`))
			if allowInbox {
				require.NoError(t, err)
				require.Contains(t, result, "original broadcast")
			} else {
				require.Error(t, err)
				require.Empty(t, result)
				if test == "missing store" {
					require.ErrorContains(t, err, "brokered message store is not configured")
				}
			}
			remaining, err := data.GetMessages(t.Context(), task.Namespace, task.Name, "root", false)
			require.NoError(t, err)
			if allowInbox {
				require.Empty(t, remaining)
			} else {
				require.Len(t, remaining, 1, "denied inbox reads must not consume broadcasts")
				outgoing, err := data.GetMessages(t.Context(), task.Namespace, "peer", "root", false)
				require.NoError(t, err)
				require.Empty(t, outgoing, "denied sends must not persist messages")
			}
		})
	}
}

func TestBrokeredMessagingReauthorizesAfterParentCleanup(t *testing.T) {
	for _, operation := range []string{"send", "inbox"} {
		t.Run(operation, func(t *testing.T) {
			task, kube, data := setupBrokeredTranscriptSearch(t)
			parentReads := 0
			reader := interceptor.NewClient(kube, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
					if err := c.Get(ctx, key, object, opts...); err != nil {
						return err
					}
					if key.Name != "root" {
						return nil
					}
					parentReads++
					if parentReads != 1 {
						return nil
					}
					cleanupCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
					defer cancel()
					if err := data.DeleteParentMessages(cleanupCtx, task.Namespace, "root"); err != nil {
						return err
					}
					parent := object.(*corev1alpha1.Task).DeepCopy()
					if err := c.Delete(ctx, parent); err != nil {
						return err
					}
					parent.UID, parent.ResourceVersion = "replacement-root-uid", ""
					if err := c.Create(ctx, parent); err != nil {
						return err
					}
					return data.SendMessage(cleanupCtx, &store.Message{
						Namespace: task.Namespace, FromTask: "peer", ToTask: "*", ParentTask: "root", Content: "replacement broadcast",
					})
				},
			})
			ctx := tools.WithToolContext(t.Context(), &tools.ToolContext{
				Brokered: true, Client: kube, Namespace: task.Namespace, TaskID: task.Name, TaskUID: string(task.UID), ParentTaskID: "root",
				MessageStore: NewTaskMessageStore(reader, data, client.ObjectKeyFromObject(task), string(task.UID), true, allowBrokeredTaskData),
			})
			var result string
			var err error
			if operation == "send" {
				result, err = tools.NewSendMessageTool().Execute(ctx, json.RawMessage(`{"to_task":"peer","content":"stale send"}`))
			} else {
				result, err = tools.NewCheckMessagesTool().Execute(ctx, json.RawMessage(`{"mark_read":true}`))
			}
			require.Error(t, err)
			require.Empty(t, result)
			require.Equal(t, 2, parentReads, "parent cleanup must trigger fresh owner-UID authorization")
			messages, err := data.GetMessages(t.Context(), task.Namespace, task.Name, "root", false)
			require.NoError(t, err)
			require.Len(t, messages, 1)
			require.Equal(t, "replacement broadcast", messages[0].Content)
			outgoing, err := data.GetMessages(t.Context(), task.Namespace, "peer", "root", false)
			require.NoError(t, err)
			require.Empty(t, outgoing)
		})
	}
}
