package client

import (
	"context"
	"net/http"
	"net/url"
)

// Category sorts downloads into a directory by extension or MIME type.
type Category struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Position   int64    `json:"position"`
	Dir        string   `json:"dir"`
	Extensions []string `json:"extensions,omitempty"`
	MIMETypes  []string `json:"mimeTypes,omitempty"`
}

// CreateCategoryRequest is the request of create-category.
type CreateCategoryRequest struct {
	Name       string   `json:"name"`
	Dir        string   `json:"dir"`
	Extensions []string `json:"extensions,omitempty"`
	MIMETypes  []string `json:"mimeTypes,omitempty"`
}

// UpdateCategoryRequest is the request of update-category; nil fields are
// left unchanged.
type UpdateCategoryRequest struct {
	Name       *string   `json:"name,omitempty"`
	Dir        *string   `json:"dir,omitempty"`
	Extensions *[]string `json:"extensions,omitempty"`
	MIMETypes  *[]string `json:"mimeTypes,omitempty"`
	Position   *int64    `json:"position,omitempty"`
}

func categoryPath(id string) string { return "/v1/categories/" + url.PathEscape(id) }

// ListCategories calls list-categories (GET /v1/categories).
func (c *Client) ListCategories(ctx context.Context) ([]*Category, error) {
	var out struct {
		Categories []*Category `json:"categories"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/categories", nil, &out); err != nil {
		return nil, err
	}
	return out.Categories, nil
}

// CreateCategory calls create-category (POST /v1/categories).
func (c *Client) CreateCategory(ctx context.Context, req CreateCategoryRequest) (*Category, error) {
	var out Category
	if err := c.do(ctx, http.MethodPost, "/v1/categories", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetCategory calls get-category (GET /v1/categories/{id}).
func (c *Client) GetCategory(ctx context.Context, id string) (*Category, error) {
	var out Category
	if err := c.do(ctx, http.MethodGet, categoryPath(id), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdateCategory calls update-category (PATCH /v1/categories/{id}).
func (c *Client) UpdateCategory(ctx context.Context, id string, req UpdateCategoryRequest) (*Category, error) {
	var out Category
	if err := c.do(ctx, http.MethodPatch, categoryPath(id), req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteCategory calls delete-category (DELETE /v1/categories/{id}).
func (c *Client) DeleteCategory(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, categoryPath(id), nil, nil)
}

// MatchCategory calls match-category (GET /v1/categories/match) and returns
// the category a file would be sorted into, or nil.
func (c *Client) MatchCategory(ctx context.Context, name, mimeType string) (*Category, error) {
	var out struct {
		Category *Category `json:"category"`
	}
	q := url.Values{"name": {name}, "mimeType": {mimeType}}
	if err := c.do(ctx, http.MethodGet, "/v1/categories/match?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return out.Category, nil
}
