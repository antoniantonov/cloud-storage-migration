package model

type Item struct {
	ID   string `json:"id"`
	Path string `json:"path"`
	Name string `json:"name"`
	Type string `json:"type"`
	Size *int64 `json:"size"`
}

type SourceFile struct {
	ID          string
	Name        string
	MIMEType    string
	Size        *int64
	DownloadURL string `json:"-"`
}

type UploadedFile struct {
	ID   string
	Size *int64
	MD5  string
}
