package workspace

import (
	"context"

	ateapipb "github.com/orka-agents/orka/internal/substratepb"
	"google.golang.org/grpc"
)

// SubstrateNativeClient uses the same authenticated connection as workspace
// execution for native Atespaces, immutable templates, Actors and Tags.
type SubstrateNativeClient struct {
	Control ateapipb.ControlClient
	conn    *grpc.ClientConn
}

func NewSubstrateNativeClient(cfg SubstrateConfig) (*SubstrateNativeClient, error) {
	conn, err := newSubstrateConnection(cfg, true)
	if err != nil {
		return nil, err
	}
	return &SubstrateNativeClient{Control: ateapipb.NewControlClient(conn), conn: conn}, nil
}

func (c *SubstrateNativeClient) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

func (c *SubstrateNativeClient) ListActorTemplates(ctx context.Context, atespace string) ([]*ateapipb.ActorTemplate, error) {
	var templates []*ateapipb.ActorTemplate
	pages := substratePages{}
	for {
		page, err := c.Control.ListActorTemplates(ctx, &ateapipb.ListActorTemplatesRequest{Atespace: atespace, PageSize: 1000, PageToken: pages.token})
		if err != nil {
			return nil, substrateControlError("list actor templates", err)
		}
		templates = append(templates, page.GetActorTemplates()...)
		more, err := pages.advance(page.GetNextPageToken())
		if err != nil {
			return nil, err
		}
		if !more {
			return templates, nil
		}
	}
}

func (c *SubstrateNativeClient) ListTags(ctx context.Context, atespace string) ([]*ateapipb.Tag, error) {
	var tags []*ateapipb.Tag
	pages := substratePages{}
	for {
		page, err := c.Control.ListTags(ctx, &ateapipb.ListTagsRequest{Atespace: atespace, PageSize: 1000, PageToken: pages.token})
		if err != nil {
			return nil, substrateControlError("list tags", err)
		}
		tags = append(tags, page.GetTags()...)
		more, err := pages.advance(page.GetNextPageToken())
		if err != nil {
			return nil, err
		}
		if !more {
			return tags, nil
		}
	}
}
