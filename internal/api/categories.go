package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/service"
)

const tagCategories = "categories"

// CategoryList is the response of list-categories.
type CategoryList struct {
	Categories []*domain.Category `json:"categories" doc:"Every category, in the order they are tried."`
}

// CreateCategoryBody is the request of create-category.
type CreateCategoryBody struct {
	Name       string   `json:"name" minLength:"1" maxLength:"100" doc:"Unique (case-insensitive) category name."`
	Dir        string   `json:"dir" minLength:"1" doc:"Directory of the category's downloads: absolute, or relative to the download directory."`
	Extensions []string `json:"extensions,omitempty" doc:"File extensions without the dot, such as mp4 or tar.gz."`
	MIMETypes  []string `json:"mimeTypes,omitempty" doc:"MIME types such as application/pdf, or families such as video/*."`
}

// UpdateCategoryBody is the request of update-category. Absent fields are
// unchanged; a present list replaces the old one.
type UpdateCategoryBody struct {
	Name       *string   `json:"name,omitempty" minLength:"1" maxLength:"100" doc:"New name."`
	Dir        *string   `json:"dir,omitempty" minLength:"1" doc:"New directory."`
	Extensions *[]string `json:"extensions,omitempty" doc:"New extensions."`
	MIMETypes  *[]string `json:"mimeTypes,omitempty" doc:"New MIME types."`
	Position   *int64    `json:"position,omitempty" minimum:"0" doc:"New place in the order (0 is tried first)."`
}

// CategoryMatch is the response of match-category.
type CategoryMatch struct {
	Category *domain.Category `json:"category,omitempty" doc:"The first matching category; absent when none matches."`
}

type categoryOutput struct {
	Body *domain.Category
}

type categoryPathInput struct {
	ID string `path:"id" doc:"Category ID."`
}

func (s *Server) registerCategories() {
	Register(s, huma.Operation{
		OperationID: "list-categories",
		Method:      http.MethodGet,
		Path:        "/v1/categories",
		Summary:     "List categories",
		Description: "Categories sort downloads into directories when the autoCategorize setting is on; the first matching one wins.",
		Tags:        []string{tagCategories},
		Errors:      []int{http.StatusServiceUnavailable},
	}, s.listCategories)

	Register(s, huma.Operation{
		OperationID:   "create-category",
		Method:        http.MethodPost,
		Path:          "/v1/categories",
		Summary:       "Create a category",
		Description:   "Adds a category at the end of the order.",
		Tags:          []string{tagCategories},
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusConflict, http.StatusUnprocessableEntity, http.StatusServiceUnavailable},
	}, s.createCategory)

	Register(s, huma.Operation{
		OperationID: "match-category",
		Method:      http.MethodGet,
		Path:        "/v1/categories/match",
		Summary:     "Find the category of a file",
		Description: "Returns the category a file with this name and MIME type would be sorted into.",
		Tags:        []string{tagCategories},
		Errors:      []int{http.StatusServiceUnavailable},
	}, s.matchCategory)

	Register(s, huma.Operation{
		OperationID: "get-category",
		Method:      http.MethodGet,
		Path:        "/v1/categories/{id}",
		Summary:     "Get a category",
		Tags:        []string{tagCategories},
		Errors:      []int{http.StatusNotFound, http.StatusServiceUnavailable},
	}, s.getCategory)

	Register(s, huma.Operation{
		OperationID: "update-category",
		Method:      http.MethodPatch,
		Path:        "/v1/categories/{id}",
		Summary:     "Update a category",
		Tags:        []string{tagCategories},
		Errors:      []int{http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusServiceUnavailable},
	}, s.updateCategory)

	Register(s, huma.Operation{
		OperationID:   "delete-category",
		Method:        http.MethodDelete,
		Path:          "/v1/categories/{id}",
		Summary:       "Delete a category",
		Description:   "Deletes a category. Files already sorted by it stay where they are.",
		Tags:          []string{tagCategories},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusNotFound, http.StatusServiceUnavailable},
	}, s.deleteCategory)
}

func (s *Server) listCategories(ctx context.Context, _ *struct{}) (*struct{ Body CategoryList }, error) {
	if s.categories == nil {
		return nil, errServicesUnavailable()
	}
	cs, err := s.categories.List(ctx)
	if err != nil {
		return nil, s.fail(ctx, "list-categories", err, locBody)
	}
	if cs == nil {
		cs = []*domain.Category{}
	}
	return &struct{ Body CategoryList }{Body: CategoryList{Categories: cs}}, nil
}

func (s *Server) createCategory(ctx context.Context, in *struct{ Body CreateCategoryBody }) (*categoryOutput, error) {
	if s.categories == nil {
		return nil, errServicesUnavailable()
	}
	c, err := s.categories.Create(ctx, service.CreateCategoryInput{
		Name: in.Body.Name, Dir: in.Body.Dir, Extensions: in.Body.Extensions, MIMETypes: in.Body.MIMETypes,
	})
	if err != nil {
		return nil, s.fail(ctx, "create-category", err, locBody)
	}
	return &categoryOutput{Body: c}, nil
}

func (s *Server) matchCategory(ctx context.Context, in *struct {
	Name     string `query:"name" doc:"File name."`
	MIMEType string `query:"mimeType" doc:"MIME type."`
},
) (*struct{ Body CategoryMatch }, error) {
	if s.categories == nil {
		return nil, errServicesUnavailable()
	}
	c, err := s.categories.Match(ctx, in.Name, in.MIMEType)
	if err != nil {
		return nil, s.fail(ctx, "match-category", err, locBody)
	}
	return &struct{ Body CategoryMatch }{Body: CategoryMatch{Category: c}}, nil
}

func (s *Server) getCategory(ctx context.Context, in *categoryPathInput) (*categoryOutput, error) {
	if s.categories == nil {
		return nil, errServicesUnavailable()
	}
	c, err := s.categories.Get(ctx, in.ID)
	if err != nil {
		return nil, s.fail(ctx, "get-category", err, locBody)
	}
	return &categoryOutput{Body: c}, nil
}

func (s *Server) updateCategory(ctx context.Context, in *struct {
	ID   string `path:"id" doc:"Category ID."`
	Body UpdateCategoryBody
},
) (*categoryOutput, error) {
	if s.categories == nil {
		return nil, errServicesUnavailable()
	}
	c, err := s.categories.Update(ctx, in.ID, service.UpdateCategoryInput{
		Name: in.Body.Name, Dir: in.Body.Dir, Extensions: in.Body.Extensions, MIMETypes: in.Body.MIMETypes, Position: in.Body.Position,
	})
	if err != nil {
		return nil, s.fail(ctx, "update-category", err, locBody)
	}
	return &categoryOutput{Body: c}, nil
}

func (s *Server) deleteCategory(ctx context.Context, in *categoryPathInput) (*struct{}, error) {
	if s.categories == nil {
		return nil, errServicesUnavailable()
	}
	if err := s.categories.Delete(ctx, in.ID); err != nil {
		return nil, s.fail(ctx, "delete-category", err, locBody)
	}
	return &struct{}{}, nil
}
