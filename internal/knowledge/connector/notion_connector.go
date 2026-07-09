package connector

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jomei/notionapi"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

// NotionConnector is a connector for importing documents from Notion.
var _ KnowledgeSourceConnector = (*NotionConnector)(nil)

type NotionConnector struct {
	tokenProvider func(context.Context) (string, error)
}

// NewNotionConnector creates a new Notion connector.
// tokenProvider should integrate with the P0-1 secure credential vault.
func NewNotionConnector(tokenProvider func(context.Context) (string, error)) *NotionConnector {
	return &NotionConnector{
		tokenProvider: tokenProvider,
	}
}

func (c *NotionConnector) ID() string {
	return "notion-api-connector"
}

func (c *NotionConnector) Name() string {
	return "Notion API Connector"
}

func (c *NotionConnector) SyncConfig() types.SyncConfig {
	return types.SyncConfig{
		DefaultInterval: 3600,  // 1 hour full sync
		SupportsWatch:   false, // Notion doesn't support easy websocket watch without heavy webhook setup
		MaxBatchSize:    50,
	}
}

// getClient obtains an authenticated Notion client
func (c *NotionConnector) getClient(ctx context.Context) (*notionapi.Client, error) {
	if c.tokenProvider == nil {
		return nil, apperr.New(apperr.CodeForbidden, "Notion token provider not configured")
	}
	token, err := c.tokenProvider(ctx)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeForbidden, "failed to get notion token", err)
	}
	return notionapi.NewClient(notionapi.Token(token)), nil
}

// List scans the Notion workspace for pages using the search API.
func (c *NotionConnector) List(ctx context.Context) ([]*types.DocumentRef, error) {
	client, err := c.getClient(ctx)
	if err != nil {
		return nil, err
	}

	var refs []*types.DocumentRef
	var cursor notionapi.Cursor

	for {
		req := &notionapi.SearchRequest{
			Filter: notionapi.SearchFilter{
				Property: "object",
				Value:    "page",
			},
			StartCursor: cursor,
		}

		res, err := client.Search.Do(ctx, req)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "failed to search notion", err)
		}

		for _, obj := range res.Results {
			page, ok := obj.(*notionapi.Page)
			if !ok {
				continue
			}

			// Extract title
			title := "Untitled"
			if page.Properties != nil {
				for _, prop := range page.Properties {
					if titleProp, ok := prop.(*notionapi.TitleProperty); ok && len(titleProp.Title) > 0 {
						title = titleProp.Title[0].PlainText
						break
					}
				}
			}

			modTime := page.LastEditedTime.Unix()

			refs = append(refs, &types.DocumentRef{
				URI:         "notion://" + page.ID.String(),
				Title:       title,
				SourceType:  "notion_page",
				ContentHash: fmt.Sprintf("%s-%d", page.ID.String(), modTime), // Simplistic hash based on edit time
				ModifiedAt:  modTime,
				Size:        0, // Unknown without fetching blocks
			})
		}

		if !res.HasMore {
			break
		}
		cursor = res.NextCursor
	}

	return refs, nil
}

// Fetch reads blocks from a Notion page to assemble the content.
func (c *NotionConnector) Fetch(ctx context.Context, ref *types.DocumentRef) (*types.SyncDocument, error) {
	client, err := c.getClient(ctx)
	if err != nil {
		return nil, err
	}

	pageID := strings.TrimPrefix(ref.URI, "notion://")

	// Fetch blocks
	var cursor notionapi.Cursor
	var contentBuilder strings.Builder

	for {
		res, err := client.Block.GetChildren(ctx, notionapi.BlockID(pageID), &notionapi.Pagination{
			StartCursor: cursor,
			PageSize:    100,
		})
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "failed to get notion blocks", err)
		}

		for _, block := range res.Results {
			text := extractBlockText(block)
			if text != "" {
				contentBuilder.WriteString(text)
				contentBuilder.WriteString("\n\n")
			}
		}

		if !res.HasMore {
			break
		}
		cursor = notionapi.Cursor(res.NextCursor)
	}

	return &types.SyncDocument{
		URI:     ref.URI,
		Title:   ref.Title,
		Content: []byte(strings.TrimSpace(contentBuilder.String())),
		Metadata: map[string]string{
			"notion_page_id": pageID,
			"fetched_at":     time.Now().Format(time.RFC3339),
		},
	}, nil
}

// extractBlockText does a basic extraction of text from common block types.
func extractBlockText(block notionapi.Block) string {
	switch b := block.(type) {
	case *notionapi.ParagraphBlock:
		return renderRichText(b.Paragraph.RichText)
	case *notionapi.Heading1Block:
		return "# " + renderRichText(b.Heading1.RichText)
	case *notionapi.Heading2Block:
		return "## " + renderRichText(b.Heading2.RichText)
	case *notionapi.Heading3Block:
		return "### " + renderRichText(b.Heading3.RichText)
	case *notionapi.BulletedListItemBlock:
		return "- " + renderRichText(b.BulletedListItem.RichText)
	case *notionapi.NumberedListItemBlock:
		return "1. " + renderRichText(b.NumberedListItem.RichText)
	case *notionapi.ToDoBlock:
		check := " "
		if b.ToDo.Checked {
			check = "x"
		}
		return fmt.Sprintf("- [%s] %s", check, renderRichText(b.ToDo.RichText))
	case *notionapi.CodeBlock:
		return fmt.Sprintf("```%s\n%s\n```", b.Code.Language, renderRichText(b.Code.RichText))
	case *notionapi.QuoteBlock:
		return "> " + renderRichText(b.Quote.RichText)
	default:
		return ""
	}
}

func renderRichText(richText []notionapi.RichText) string {
	var out strings.Builder
	for _, rt := range richText {
		out.WriteString(rt.PlainText)
	}
	return out.String()
}

func (c *NotionConnector) Watch(ctx context.Context) (<-chan types.ChangeEvent, error) {
	// Not supported via push; relies on periodic Sync() by sync_scheduler.
	out := make(chan types.ChangeEvent)
	concurrent.SafeGo(ctx, "knowledge.connector.notion_watch", func(ctx context.Context) {
		defer close(out)
		<-ctx.Done()
	})
	return out, nil
}
