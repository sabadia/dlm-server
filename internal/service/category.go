package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/storage"
)

// CategoryService manages the categories that sort downloads into
// directories. A category is chosen by the first rule that matches, in
// position order.
type CategoryService struct {
	d Deps
}

// NewCategoryService creates a CategoryService.
func NewCategoryService(d Deps) *CategoryService {
	return &CategoryService{d: d.withDefaults()}
}

// CreateCategoryInput are the fields of a new category.
type CreateCategoryInput struct {
	Name       string
	Dir        string
	Extensions []string
	MIMETypes  []string
}

// UpdateCategoryInput changes a category. Nil fields are left unchanged; a
// non-nil list replaces the old one.
type UpdateCategoryInput struct {
	Name       *string
	Dir        *string
	Extensions *[]string
	MIMETypes  *[]string
	// Position moves the category in the order (0 is tried first).
	Position *int64
}

// normalizeRules lower-cases the rules and drops dots and blanks the user
// is likely to type: ".MP4" is the extension "mp4".
func normalizeRules(extensions, mimeTypes []string) (exts, mimes []string) {
	for _, e := range extensions {
		if e = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(e), ".")); e != "" {
			exts = append(exts, e)
		}
	}
	for _, m := range mimeTypes {
		if m = strings.ToLower(strings.TrimSpace(m)); m != "" {
			mimes = append(mimes, m)
		}
	}
	return exts, mimes
}

// List returns every category in the order they are tried.
func (s *CategoryService) List(ctx context.Context) ([]*domain.Category, error) {
	cs, err := s.d.Store.Categories().List(ctx)
	if err != nil {
		return nil, fmt.Errorf("service: list categories: %w", err)
	}
	return cs, nil
}

// Get returns one category.
func (s *CategoryService) Get(ctx context.Context, id string) (*domain.Category, error) {
	c, err := s.d.Store.Categories().Get(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("service: get category: %w", err)
	}
	return c, nil
}

// Create adds a category at the end of the order.
func (s *CategoryService) Create(ctx context.Context, in CreateCategoryInput) (*domain.Category, error) {
	exts, mimes := normalizeRules(in.Extensions, in.MIMETypes)
	c := &domain.Category{
		ID: s.d.NewID(), Name: strings.TrimSpace(in.Name), Dir: strings.TrimSpace(in.Dir), Extensions: exts, MIMETypes: mimes,
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("service: create category: %w", err)
	}
	err := s.d.Store.WithTx(ctx, func(tx storage.Store) error {
		pos, err := tx.Categories().NextPosition(ctx)
		if err != nil {
			return err //nolint:wrapcheck // wrapped below
		}
		c.Position = pos
		return tx.Categories().Create(ctx, c) //nolint:wrapcheck // wrapped below
	})
	if err != nil {
		return nil, fmt.Errorf("service: create category: %w", err)
	}
	return c, nil
}

// Update changes a category.
func (s *CategoryService) Update(ctx context.Context, id string, in UpdateCategoryInput) (*domain.Category, error) {
	var c *domain.Category
	err := s.d.Store.WithTx(ctx, func(tx storage.Store) error {
		var err error
		if c, err = tx.Categories().Get(ctx, id); err != nil {
			return err //nolint:wrapcheck // wrapped below
		}
		if in.Name != nil {
			c.Name = strings.TrimSpace(*in.Name)
		}
		if in.Dir != nil {
			c.Dir = strings.TrimSpace(*in.Dir)
		}
		if in.Extensions != nil || in.MIMETypes != nil {
			ext, mime := c.Extensions, c.MIMETypes
			if in.Extensions != nil {
				ext = *in.Extensions
			}
			if in.MIMETypes != nil {
				mime = *in.MIMETypes
			}
			c.Extensions, c.MIMETypes = normalizeRules(ext, mime)
		}
		if in.Position != nil {
			if *in.Position < 0 {
				return domain.InvalidArgument("category position must be >= 0")
			}
			c.Position = *in.Position
		}
		if err := c.Validate(); err != nil {
			return err //nolint:wrapcheck // wrapped below
		}
		return tx.Categories().Update(ctx, c) //nolint:wrapcheck // wrapped below
	})
	if err != nil {
		return nil, fmt.Errorf("service: update category: %w", err)
	}
	return c, nil
}

// Delete removes a category. Tasks already sorted by it keep their files where
// they are.
func (s *CategoryService) Delete(ctx context.Context, id string) error {
	if err := s.d.Store.Categories().Delete(ctx, id); err != nil {
		return fmt.Errorf("service: delete category: %w", err)
	}
	return nil
}

// Match returns the category a file with this name and MIME type belongs to,
// or nil when none does.
func (s *CategoryService) Match(ctx context.Context, name, mimeType string) (*domain.Category, error) {
	cs, err := s.d.Store.Categories().List(ctx)
	if err != nil {
		return nil, fmt.Errorf("service: match category: %w", err)
	}
	for _, c := range cs {
		if c.Matches(name, mimeType) {
			return c, nil
		}
	}
	return nil, nil //nolint:nilnil // no category is not an error
}
