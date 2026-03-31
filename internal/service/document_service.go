// Package service 包含了应用的业务逻辑层。
package service

import (
	"WeaveKnow/internal/config"
	"WeaveKnow/internal/model"
	"WeaveKnow/internal/repository"
	"WeaveKnow/pkg/storage"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/url"
	"path/filepath"
	"strings"

	"WeaveKnow/pkg/tika"
	"github.com/minio/minio-go/v7"
)

// FileUploadDTO 是一个数据传输对象，用于在返回给前端时隐藏一些字段并添加额外信息。
type FileUploadDTO struct {
	model.FileUpload
	OrgTagName string `json:"orgTagName"`
}

// DownloadInfoDTO 封装了文件下载链接所需的信息。
type DownloadInfoDTO struct {
	FileName    string `json:"fileName"`
	FileMD5     string `json:"fileMd5"`
	DownloadURL string `json:"downloadUrl"`
	FileSize    int64  `json:"fileSize"`
}

// PreviewInfoDTO 封装了文件预览所需的信息。
type PreviewInfoDTO struct {
	FileName         string `json:"fileName"`
	FileMD5          string `json:"fileMd5"`
	Content          string `json:"content,omitempty"`
	FileSize         int64  `json:"fileSize"`
	PreviewType      string `json:"previewType"`
	PreviewURL       string `json:"previewUrl,omitempty"`
	SourceURL        string `json:"sourceUrl,omitempty"`
	SinglePageMode   bool   `json:"singlePageMode,omitempty"`
	SourcePageNumber int    `json:"sourcePageNumber,omitempty"`
}

type FileStreamDTO struct {
	FileName    string
	FileMD5     string
	FileSize    int64
	ContentType string
	Reader      io.ReadCloser
}

// DocumentService 接口定义了文档管理相关的业务操作。
type DocumentService interface {
	ListAccessibleFiles(user *model.User) ([]model.FileUpload, error)
	ListUploadedFiles(userID uint) ([]FileUploadDTO, error)
	DeleteDocument(fileMD5 string, user *model.User) error
	GenerateDownloadURL(fileName string, fileMD5 string, user *model.User) (*DownloadInfoDTO, error)
	GetFilePreviewContent(fileName string, fileMD5 string, pageNumber int, user *model.User) (*PreviewInfoDTO, error)
	GetFileStream(fileName string, fileMD5 string, user *model.User) (*FileStreamDTO, error)
}

type documentService struct {
	uploadRepo repository.UploadRepository
	userRepo   repository.UserRepository
	orgTagRepo repository.OrgTagRepository // 新增依赖
	minioCfg   config.MinIOConfig
	tikaClient *tika.Client // 新增依赖
}

// NewDocumentService 创建一个新的 DocumentService 实例。
func NewDocumentService(uploadRepo repository.UploadRepository, userRepo repository.UserRepository, orgTagRepo repository.OrgTagRepository, minioCfg config.MinIOConfig, tikaClient *tika.Client) DocumentService {
	return &documentService{
		uploadRepo: uploadRepo,
		userRepo:   userRepo,
		orgTagRepo: orgTagRepo,
		minioCfg:   minioCfg,
		tikaClient: tikaClient,
	}
}

// ListAccessibleFiles 获取用户可访问的文件列表。
func (s *documentService) ListAccessibleFiles(user *model.User) ([]model.FileUpload, error) {
	orgTags := strings.Split(user.OrgTags, ",")
	return s.uploadRepo.FindAccessibleFiles(user.ID, orgTags)
}

// ListUploadedFiles 获取用户自己上传的文件列表，并附加组织标签名称。
func (s *documentService) ListUploadedFiles(userID uint) ([]FileUploadDTO, error) {
	files, err := s.uploadRepo.FindFilesByUserID(userID)
	if err != nil {
		return nil, err
	}

	dtos, err := s.mapFileUploadsToDTOs(files)
	if err != nil {
		return nil, err
	}

	return dtos, nil
}

// DeleteDocument 删除一个文档。
func (s *documentService) DeleteDocument(fileMD5 string, user *model.User) error {
	record, err := s.uploadRepo.GetFileUploadRecord(fileMD5, user.ID)
	if err != nil {
		return errors.New("文件不存在或不属于该用户")
	}

	if record.UserID != user.ID && user.Role != "ADMIN" {
		return errors.New("没有权限删除此文件")
	}

	objectName := fmt.Sprintf("merged/%s", record.FileName)
	err = storage.MinioClient.RemoveObject(context.Background(), s.minioCfg.BucketName, objectName, minio.RemoveObjectOptions{})
	if err != nil {
		// Log or ignore error, but proceed to delete DB record
	}

	// 从数据库删除记录
	return s.uploadRepo.DeleteFileUploadRecord(fileMD5, record.UserID)
}

// GenerateDownloadURL 生成文件的临时下载链接。
func (s *documentService) GenerateDownloadURL(fileName string, fileMD5 string, user *model.User) (*DownloadInfoDTO, error) {
	targetFile, err := s.resolveAccessibleFile(fileName, fileMD5, user)
	if err != nil {
		return nil, errors.New("文件不存在或无权访问")
	}

	return &DownloadInfoDTO{
		FileName:    targetFile.FileName,
		FileMD5:     targetFile.FileMD5,
		DownloadURL: s.buildInlinePreviewPath(targetFile, 0, true),
		FileSize:    targetFile.TotalSize,
	}, nil
}

// GetFilePreviewContent 获取文件的纯文本预览内容。
func (s *documentService) GetFilePreviewContent(fileName string, fileMD5 string, pageNumber int, user *model.User) (*PreviewInfoDTO, error) {
	targetFile, err := s.resolveAccessibleFile(fileName, fileMD5, user)
	if err != nil {
		return nil, errors.New("文件不存在或无权访问")
	}

	ext := strings.ToLower(filepath.Ext(targetFile.FileName))
	switch ext {
	case ".pdf":
		return &PreviewInfoDTO{
			FileName:         targetFile.FileName,
			FileMD5:          targetFile.FileMD5,
			FileSize:         targetFile.TotalSize,
			PreviewType:      "pdf",
			PreviewURL:       s.buildInlinePreviewPath(targetFile, pageNumber, false),
			SourceURL:        s.buildInlinePreviewPath(targetFile, pageNumber, false),
			SourcePageNumber: pageNumber,
		}, nil
	case ".png", ".jpg", ".jpeg", ".gif", ".bmp", ".webp", ".svg":
		return &PreviewInfoDTO{
			FileName:    targetFile.FileName,
			FileMD5:     targetFile.FileMD5,
			FileSize:    targetFile.TotalSize,
			PreviewType: "image",
			PreviewURL:  s.buildInlinePreviewPath(targetFile, pageNumber, false),
			SourceURL:   s.buildInlinePreviewPath(targetFile, pageNumber, false),
		}, nil
	}

	object, err := storage.MinioClient.GetObject(context.Background(), s.minioCfg.BucketName, fmt.Sprintf("merged/%s", targetFile.FileName), minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	defer object.Close()

	content, err := s.tikaClient.ExtractText(object, targetFile.FileName)
	if err == nil && strings.TrimSpace(content) != "" {
		return &PreviewInfoDTO{
			FileName:    targetFile.FileName,
			FileMD5:     targetFile.FileMD5,
			Content:     content,
			FileSize:    targetFile.TotalSize,
			PreviewType: "text",
			SourceURL:   s.buildInlinePreviewPath(targetFile, pageNumber, false),
		}, nil
	}

	return &PreviewInfoDTO{
		FileName:    targetFile.FileName,
		FileMD5:     targetFile.FileMD5,
		FileSize:    targetFile.TotalSize,
		PreviewType: "download",
		SourceURL:   s.buildInlinePreviewPath(targetFile, pageNumber, false),
	}, nil
}

func (s *documentService) GetFileStream(fileName string, fileMD5 string, user *model.User) (*FileStreamDTO, error) {
	targetFile, err := s.resolveAccessibleFile(fileName, fileMD5, user)
	if err != nil {
		return nil, errors.New("文件不存在或无权访问")
	}

	objectName := fmt.Sprintf("merged/%s", targetFile.FileName)
	object, err := storage.MinioClient.GetObject(context.Background(), s.minioCfg.BucketName, objectName, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}

	contentType := mime.TypeByExtension(strings.ToLower(filepath.Ext(targetFile.FileName)))
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	return &FileStreamDTO{
		FileName:    targetFile.FileName,
		FileMD5:     targetFile.FileMD5,
		FileSize:    targetFile.TotalSize,
		ContentType: contentType,
		Reader:      object,
	}, nil
}

func (s *documentService) resolveAccessibleFile(fileName string, fileMD5 string, user *model.User) (*model.FileUpload, error) {
	files, err := s.ListAccessibleFiles(user)
	if err != nil {
		return nil, err
	}

	trimmedMD5 := strings.TrimSpace(fileMD5)
	trimmedName := strings.TrimSpace(fileName)
	for i := range files {
		switch {
		case trimmedMD5 != "" && files[i].FileMD5 == trimmedMD5:
			return &files[i], nil
		case trimmedMD5 == "" && trimmedName != "" && files[i].FileName == trimmedName:
			return &files[i], nil
		}
	}

	return nil, errors.New("文件不存在或无权访问")
}

func (s *documentService) buildInlinePreviewPath(targetFile *model.FileUpload, pageNumber int, forceDownload bool) string {
	values := url.Values{}
	values.Set("fileMd5", targetFile.FileMD5)
	values.Set("fileName", targetFile.FileName)
	if pageNumber > 0 {
		values.Set("pageNumber", fmt.Sprintf("%d", pageNumber))
	}
	if forceDownload {
		values.Set("download", "1")
	}
	return "/api/v1/documents/raw?" + values.Encode()
}

func (s *documentService) mapFileUploadsToDTOs(files []model.FileUpload) ([]FileUploadDTO, error) {
	if len(files) == 0 {
		return []FileUploadDTO{}, nil
	}

	// To avoid N+1 queries, get all unique org tag IDs first
	tagIDs := make(map[string]struct{})
	for _, file := range files {
		if file.OrgTag != "" {
			tagIDs[file.OrgTag] = struct{}{}
		}
	}

	tagIDList := make([]string, 0, len(tagIDs))
	for id := range tagIDs {
		tagIDList = append(tagIDList, id)
	}

	tags, err := s.orgTagRepo.FindBatchByIDs(tagIDList)
	if err != nil {
		return nil, err
	}

	tagMap := make(map[string]string)
	for _, tag := range tags {
		tagMap[tag.TagID] = tag.Name
	}

	dtos := make([]FileUploadDTO, len(files))
	for i, file := range files {
		dtos[i] = FileUploadDTO{
			FileUpload: file,
			OrgTagName: tagMap[file.OrgTag], // Will be empty string if not found
		}
	}

	return dtos, nil
}
