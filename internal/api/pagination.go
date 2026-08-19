/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"

	"github.com/gofiber/fiber/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

const (
	// DefaultLimit is the default number of items per page
	DefaultLimit = 100

	// MaxLimit is the maximum number of items per page
	MaxLimit = 500
)

// Pagination holds pagination parameters
type Pagination struct {
	Limit    int64
	Continue string
}

// ParsePagination parses pagination parameters from query strings
func ParsePagination(limitStr, continueToken string) (*Pagination, error) {
	p := &Pagination{
		Limit:    DefaultLimit,
		Continue: continueToken,
	}

	if limitStr != "" {
		limit, err := strconv.ParseInt(limitStr, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid limit parameter: %w", err)
		}
		if limit < 1 {
			return nil, fmt.Errorf("limit must be at least 1")
		}
		if limit > MaxLimit {
			limit = MaxLimit
		}
		p.Limit = limit
	}

	return p, nil
}

func queryArgumentPresent(c fiber.Ctx, name string) bool {
	return c.Request().URI().QueryArgs().Has(name)
}

func taskPaginationRequested(c fiber.Ctx) (bool, error) {
	if !queryArgumentPresent(c, "paginate") {
		return false, nil
	}
	paginate, err := strconv.ParseBool(c.Query("paginate", ""))
	if err != nil {
		return false, fmt.Errorf("invalid paginate parameter: must be true or false")
	}
	return paginate, nil
}

type taskPaginationParams struct {
	limit      string
	continueID string
	requested  bool
}

func parseTaskPaginationParams(c fiber.Ctx) (taskPaginationParams, error) {
	params := taskPaginationParams{
		limit:      c.Query("limit", "100"),
		continueID: c.Query("continue", ""),
	}
	requested, err := taskPaginationRequested(c)
	if err != nil {
		return taskPaginationParams{}, err
	}
	params.requested = requested
	if params.continueID != "" && !params.requested {
		return taskPaginationParams{}, fmt.Errorf("continue requires paginate=true")
	}
	if params.limit == "0" && params.requested {
		return taskPaginationParams{}, fmt.Errorf("paginate=true cannot be used with limit=0")
	}
	if params.limit != "0" {
		if _, err := ParsePagination(params.limit, ""); err != nil {
			return taskPaginationParams{}, err
		}
	}
	return params, nil
}

func paginationLimitInt(limit int64) (int, error) {
	if limit < 1 || limit > int64(MaxLimit) {
		return 0, fmt.Errorf("pagination limit %d is out of range", limit)
	}
	return int(limit), nil
}

const taskListCursorVersion = 2

type taskListCursor struct {
	Version   int    `json:"v"`
	Namespace string `json:"n"`
	Offset    int    `json:"o"`
	Snapshot  string `json:"s"`
}

func encodeTaskListCursor(cursor taskListCursor) (string, error) {
	data, err := json.Marshal(cursor)
	if err != nil {
		return "", fmt.Errorf("encode tasks continue cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func decodeTaskListCursor(raw, namespace string) (taskListCursor, error) {
	cursor := taskListCursor{Version: taskListCursorVersion, Namespace: namespace}
	if raw == "" {
		return cursor, nil
	}
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return taskListCursor{}, fmt.Errorf("invalid tasks continue cursor: malformed encoding")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cursor); err != nil {
		return taskListCursor{}, fmt.Errorf("invalid tasks continue cursor: malformed payload")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return taskListCursor{}, fmt.Errorf("invalid tasks continue cursor: trailing payload")
	}
	if cursor.Version != taskListCursorVersion || cursor.Namespace != namespace || cursor.Offset < 0 || cursor.Snapshot == "" {
		return taskListCursor{}, fmt.Errorf("invalid tasks continue cursor: cursor does not match this request")
	}
	return cursor, nil
}

func taskListSnapshot(tasks []corev1alpha1.Task) (string, error) {
	type stableTask struct {
		Namespace   string                `json:"namespace"`
		Name        string                `json:"name"`
		UID         string                `json:"uid"`
		Generation  int64                 `json:"generation"`
		Labels      map[string]string     `json:"labels,omitempty"`
		Annotations map[string]string     `json:"annotations,omitempty"`
		Spec        corev1alpha1.TaskSpec `json:"spec"`
	}
	stable := make([]stableTask, 0, len(tasks))
	for i := range tasks {
		stable = append(stable, stableTask{
			Namespace: tasks[i].Namespace, Name: tasks[i].Name, UID: string(tasks[i].UID),
			Generation: tasks[i].Generation, Labels: tasks[i].Labels, Annotations: tasks[i].Annotations, Spec: tasks[i].Spec,
		})
	}
	data, err := json.Marshal(stable)
	if err != nil {
		return "", fmt.Errorf("encode task list snapshot: %w", err)
	}
	sum := sha256.Sum256(data)
	return base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

func logicalTaskListPage(namespace string, pageSize int, rawCursor string, tasks []corev1alpha1.Task) (ListResponse, error) {
	tasks = append([]corev1alpha1.Task(nil), tasks...)
	sort.Slice(tasks, func(i, j int) bool {
		if tasks[i].Namespace == tasks[j].Namespace {
			if tasks[i].Name == tasks[j].Name {
				return string(tasks[i].UID) < string(tasks[j].UID)
			}
			return tasks[i].Name < tasks[j].Name
		}
		return tasks[i].Namespace < tasks[j].Namespace
	})
	cursor, err := decodeTaskListCursor(rawCursor, namespace)
	if err != nil {
		return ListResponse{}, fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	snapshot, err := taskListSnapshot(tasks)
	if err != nil {
		return ListResponse{}, fiber.NewError(fiber.StatusInternalServerError, err.Error())
	}
	if rawCursor == "" {
		cursor.Snapshot = snapshot
	} else if cursor.Snapshot != snapshot {
		return ListResponse{}, fiber.NewError(fiber.StatusGone, "tasks continue cursor expired; restart the list")
	}
	if cursor.Offset > len(tasks) {
		return ListResponse{}, fiber.NewError(fiber.StatusBadRequest, "invalid tasks continue cursor: cursor does not match this request")
	}
	end := min(len(tasks), cursor.Offset+pageSize)
	items := append(make([]corev1alpha1.Task, 0, end-cursor.Offset), tasks[cursor.Offset:end]...)
	metadata := ListMeta{}
	if end < len(tasks) {
		metadata.Continue, err = encodeTaskListCursor(taskListCursor{
			Version: taskListCursorVersion, Namespace: namespace, Offset: end, Snapshot: snapshot,
		})
		if err != nil {
			return ListResponse{}, fiber.NewError(fiber.StatusInternalServerError, err.Error())
		}
		remaining := int64(len(tasks) - end)
		metadata.RemainingItemCount = &remaining
	}
	return ListResponse{Items: items, Metadata: metadata}, nil
}

func paginationListError(resource string, err error) error {
	if apierrors.IsResourceExpired(err) {
		return fiber.NewError(fiber.StatusGone, fmt.Sprintf("%s continue cursor expired; restart the list", resource))
	}
	if apierrors.IsBadRequest(err) {
		return fiber.NewError(fiber.StatusBadRequest, fmt.Sprintf("invalid %s continue cursor", resource))
	}
	return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list %s: %v", resource, err))
}
