package api

import (
	"LearningGuide/file_api/config"
	"LearningGuide/file_api/global"
	FileProto "LearningGuide/file_api/proto/.FileProto"
	"LearningGuide/file_api/utils"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/OuterCyrex/ChatGLM_sdk"
	"github.com/OuterCyrex/Gorra/GorraAPI"
	handleGrpc "github.com/OuterCyrex/Gorra/GorraAPI/resp"
	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss"
	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss/credentials"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"go.uber.org/zap"
	"golang.org/x/xerrors"
	"net/http"
	"path"
	"path/filepath"
	"strconv"
	"time"
)

func FileList(c *gin.Context) {
	ctx := GorraAPI.RawContextWithSpan(c)

	fileName := c.DefaultQuery("file_name", "")
	fileType := c.DefaultQuery("file_type", "")
	err, userId, courseId, pageNum, pageSize := func() (error, int, int, int, int) {
		userId, err := strconv.Atoi(c.DefaultQuery("user_id", "0"))
		if err != nil {
			return err, 0, 0, 0, 0
		}
		courseId, err := strconv.Atoi(c.DefaultQuery("course_id", "0"))
		if err != nil {
			return err, 0, 0, 0, 0
		}
		pageNum, err := strconv.Atoi(c.DefaultQuery("pageNum", "0"))
		if err != nil {
			return err, 0, 0, 0, 0
		}
		pageSize, err := strconv.Atoi(c.DefaultQuery("pageSize", "10"))
		if err != nil {
			return err, 0, 0, 0, 0
		}
		return nil, userId, courseId, pageNum, pageSize
	}()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"msg": "无效查询参数",
		})
		return
	}

	resp, err := global.FileSrvClient.FileList(ctx, &FileProto.FileFilterRequest{
		FileName: fileName,
		FileType: fileType,
		UserId:   int32(userId),
		CourseId: int32(courseId),
		PageNum:  int32(pageNum),
		PageSize: int32(pageSize),
	})

	if err != nil {
		handleGrpc.HandleGrpcErrorToHttp(err, c)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"total": resp.Total,
		"data":  resp.Data,
	})
}

func UploadFile(c *gin.Context) {
	ctx := GorraAPI.RawContextWithSpan(c)

	fileHeader, err := c.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"msg": "无效的文件类型",
		})
		return
	}

	userId, err := strconv.Atoi(c.DefaultPostForm("user_id", "0"))
	if err != nil || userId <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{
			"msg": "无效的ID参数",
		})
		return
	}

	courseId, err := strconv.Atoi(c.DefaultPostForm("course_id", "0"))

	if err != nil || courseId <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{
			"msg": "无效的ID参数",
		})
		return
	}

	if fileHeader.Size >= 5242880 {
		c.JSON(http.StatusBadRequest, gin.H{
			"msg": "文件大小超过5MB",
		})
		return
	}

	file, err := fileHeader.Open()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"msg": "无效的文件类型",
		})
		return
	}

	client := getOssClient(global.ServerConfig.AliyunOss)

	uuid := generateUniqueID(fileHeader.Filename, userId, courseId)

	request := &oss.PutObjectRequest{
		Bucket: oss.Ptr(global.ServerConfig.AliyunOss.FileBucketName),
		Key:    oss.Ptr(uuid),
		Body:   file,
		Metadata: map[string]string{
			"Content-Disposition": `attachment; filename="` + fileHeader.Filename + `"`,
		},
	}

	_, err = client.PutObject(context.TODO(), request)

	if err != nil {
		zap.S().Errorf("文件上传失败: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"msg": "文件上传失败",
		})
		return
	}

	glm := ChatGLM_sdk.NewClient(global.ServerConfig.ChatGLM.AccessKey)

	file, err = fileHeader.Open()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"msg": "无效的文件类型",
		})
		return
	}

	content, err := utils.ReadFile(file, fileHeader.Filename)
	if errors.Is(err, utils.ErrInvalidType) {
		c.JSON(http.StatusBadRequest, gin.H{
			"msg": fmt.Sprintf("无效的文件类型: %s", path.Ext(fileHeader.Filename)),
		})
		return
	} else if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"msg": "文件上传失败",
		})
		zap.S().Errorf("read from file failed: %v", err)
		return
	}

	descId, err := glm.SendAsync(ChatGLM_sdk.NewContext(), content+getPrompt())

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"msg": "解析文件失败",
		})
		return
	}

	resp, err := global.FileSrvClient.CreateFile(ctx, &FileProto.CreateFileRequest{
		FileName: fileHeader.Filename,
		FileType: filepath.Ext(fileHeader.Filename),
		FileSize: fileHeader.Size,
		OssUrl:   uuid,
		Desc:     descId,
		UserId:   int32(userId),
		CourseId: int32(courseId),
	})

	if err != nil {
		handleGrpc.HandleGrpcErrorToHttp(err, c)
		return
	}

	global.RDB.Del(context.TODO(), fmt.Sprintf("%d", resp.Id))
	_, err = getFileInfo(ctx, int(resp.Id))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"msg": "文件上传失败",
		})
		zap.S().Errorf("set fileInfo to redis failed: %v", err)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"id": resp.Id,
	})
}

func GetFileDesc(c *gin.Context) {
	ctx := GorraAPI.RawContextWithSpan(c)

	id := c.Param("id")

	fileId, err := strconv.Atoi(id)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"msg": "无效路径参数",
		})
		return
	}

	resp, err := getFileInfo(ctx, fileId)
	if err != nil {
		handleGrpc.HandleGrpcErrorToHttp(err, c)
		return
	}

	glm := ChatGLM_sdk.NewClient(global.ServerConfig.ChatGLM.AccessKey)
	Result := glm.GetAsyncMessage(ChatGLM_sdk.NewContext(), resp.Desc)

	if errors.Is(Result.Error, ChatGLM_sdk.ErrResultProcessing) {
		c.JSON(http.StatusAccepted, gin.H{
			"msg": "GLM正在生成中",
		})
		return
	} else if Result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"msg": "服务器内部错误",
		})
		zap.S().Errorf("get result from glm failed: %v", Result.Error)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"desc": Result.Message[0].Content,
	})
}

func GetFileDetail(c *gin.Context) {
	ctx := GorraAPI.RawContextWithSpan(c)

	id := c.Param("id")

	fileId, err := strconv.Atoi(id)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"msg": "无效路径参数",
		})
		return
	}

	resp, err := getFileInfo(ctx, fileId)

	if err != nil {
		handleGrpc.HandleGrpcErrorToHttp(err, c)
		return
	}

	c.JSON(http.StatusOK, resp)
}

func DownloadFile(c *gin.Context) {
	ctx := GorraAPI.RawContextWithSpan(c)

	id := c.Param("id")

	fileId, err := strconv.Atoi(id)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"msg": "无效路径参数",
		})
		return
	}

	resp, err := getFileInfo(ctx, fileId)

	if err != nil {
		handleGrpc.HandleGrpcErrorToHttp(err, c)
		return
	}

	client := getOssClient(global.ServerConfig.AliyunOss)

	expiration := time.Now().Add(1 * time.Hour)

	req := &oss.GetObjectRequest{
		Bucket: oss.Ptr(global.ServerConfig.AliyunOss.FileBucketName),
		Key:    oss.Ptr(resp.OssUrl),
		RequestCommon: oss.RequestCommon{
			Parameters: map[string]string{
				"response-content-disposition": `attachment; filename="` + resp.FileName + `"`,
			},
		},
	}

	signedURL, err := client.Presign(context.TODO(), req, oss.PresignExpiration(expiration))

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"msg": "服务器内部错误",
		})
		zap.S().Errorf("get presign url from oss failed: %v", err)
		return
	}

	c.JSON(http.StatusOK, signedURL)
}

func UpdateFileDesc(c *gin.Context) {
	ctx := GorraAPI.RawContextWithSpan(c)

	id := c.Param("id")

	fileId, err := strconv.Atoi(id)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"msg": "无效路径参数",
		})
		return
	}

	resp, err := getFileInfo(ctx, fileId)

	if err != nil {
		handleGrpc.HandleGrpcErrorToHttp(err, c)
		return
	}

	client := getOssClient(global.ServerConfig.AliyunOss)

	req := &oss.GetObjectRequest{
		Bucket: oss.Ptr(global.ServerConfig.AliyunOss.FileBucketName),
		Key:    oss.Ptr(resp.OssUrl),
	}

	result, err := client.GetObject(context.TODO(), req)

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"msg": "服务器内部错误",
		})
		zap.S().Errorf("get object from oss failed: %v", err)
		return
	}

	file, err := utils.ReadFile(result.Body, resp.FileName)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"msg": "服务器内部错误",
		})
		zap.S().Errorf("read from file failed: %v", err)
		return
	}

	glm := ChatGLM_sdk.NewClient(global.ServerConfig.ChatGLM.AccessKey)

	descId, err := glm.SendAsync(ChatGLM_sdk.NewContext(), file+getPrompt())

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"msg": "解析文件失败",
		})
		return
	}

	_, err = global.FileSrvClient.UpdateFile(ctx, &FileProto.UpdateFileRequest{
		Id:   int32(fileId),
		Desc: descId,
	})

	if err != nil {
		handleGrpc.HandleGrpcErrorToHttp(err, c)
		return
	}

	global.RDB.Del(context.Background(), fmt.Sprintf("%d", fileId))

	c.JSON(http.StatusOK, gin.H{
		"Desc": descId,
	})
}

func DeleteFile(c *gin.Context) {
	ctx := GorraAPI.RawContextWithSpan(c)

	id := c.Param("id")

	fileId, err := strconv.Atoi(id)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"msg": "无效路径参数",
		})
		return
	}

	resp, err := getFileInfo(ctx, fileId)

	if err != nil {
		handleGrpc.HandleGrpcErrorToHttp(err, c)
		return
	}

	_, err = global.FileSrvClient.DeleteFile(ctx, &FileProto.DeleteFileRequest{Id: int32(fileId)})
	if err != nil {
		handleGrpc.HandleGrpcErrorToHttp(err, c)
		return
	}

	global.RDB.Del(ctx, fmt.Sprintf("%d", fileId))

	client := getOssClient(global.ServerConfig.AliyunOss)

	_, err = client.DeleteObject(ctx, &oss.DeleteObjectRequest{
		Bucket: oss.Ptr(global.ServerConfig.AliyunOss.FileBucketName),
		Key:    oss.Ptr(resp.OssUrl),
	})

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"msg": "服务器内部错误",
		})
		zap.S().Errorf("oss delete object failed: %v", err)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"msg": "删除成功",
	})
}

func getOssClient(config config.OssConfig) *oss.Client {
	cfg := oss.LoadDefaultConfig().WithCredentialsProvider(
		credentials.NewStaticCredentialsProvider(config.AccessKey, config.SecretKey, "")).
		WithRegion(config.Region)

	return oss.NewClient(cfg)
}

func getFileInfo(ctx context.Context, id int) (*FileProto.FileInfoResponse, error) {
	result, err := global.RDB.Get(ctx, fmt.Sprintf("%d", id)).Result()

	if errors.Is(err, redis.Nil) {
		resp, rpcErr := global.FileSrvClient.GetFileDetail(ctx, &FileProto.FileDetailRequest{Id: int32(id)})
		if rpcErr != nil {
			return nil, rpcErr
		}

		fileInfo, err := json.Marshal(resp)
		if err != nil {
			return nil, err
		}

		err = global.RDB.Set(ctx, fmt.Sprintf("%d", id), fileInfo, 20*time.Minute).Err()
		if err != nil {
			return nil, xerrors.Errorf("failed to set file name in Redis: %v", err)
		}

		return resp, nil
	} else if err != nil {
		return nil, xerrors.Errorf("failed to get file name in Redis: %v", err)
	}

	var fileInfo FileProto.FileInfoResponse

	err = json.Unmarshal([]byte(result), &fileInfo)
	if err != nil {
		return nil, err
	}

	return &fileInfo, nil
}

func getPrompt() string {
	return `请简要分析一下上述内容，回答的字数限制在500字左右`
}

func generateUniqueID(fileName string, userId int, courseId int) string {
	return fmt.Sprintf("%d-%d-%s", userId, courseId, fileName)
}
