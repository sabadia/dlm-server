package domain

// Resource is the result of resolving a URL: what would be downloaded.
type Resource struct {
	// Name is the suggested name (file name, or folder name for multi-file).
	Name string `json:"name"`
	// Size is the total size in bytes; -1 when unknown.
	Size           int64  `json:"size"`
	RangeSupported bool   `json:"rangeSupported"`
	Files          []File `json:"files,omitempty"`
}

// File is one file inside a resource (several for torrents and archives).
type File struct {
	Index int `json:"index"`
	// Path is the relative, slash-separated directory inside the resource
	// ("" for the root).
	Path string `json:"path,omitempty"`
	Name string `json:"name"`
	// Size in bytes; -1 when unknown.
	Size     int64 `json:"size"`
	Selected bool  `json:"selected"`
}

// SelectedSize returns the number of bytes that will be downloaded: the sum
// of the selected files' sizes, or Size when the resource lists no files.
// It returns -1 when any selected file's size (or Size) is unknown.
func (r *Resource) SelectedSize() int64 {
	if len(r.Files) == 0 {
		if r.Size < 0 {
			return -1
		}
		return r.Size
	}
	var total int64
	for _, f := range r.Files {
		if !f.Selected {
			continue
		}
		if f.Size < 0 {
			return -1
		}
		total += f.Size
	}
	return total
}
