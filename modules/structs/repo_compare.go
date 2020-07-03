package structs

type CompareResult struct {
	Diffs []*DiffFile `json:"diffs"`
}

type DiffFile struct {
	OldPath   string `json:"old_path"`
	NewPath   string `json:"new_path"`
	Diff      string `json:"diff"`
	IsCreated bool   `json:"is_created"`
	IsRenamed bool   `json:"is_renamed"`
	IsDeleted bool   `json:"is_deleted"`
}
