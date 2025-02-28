package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/TicketsBot/GoPanel/app"
	"github.com/TicketsBot/GoPanel/botcontext"
	"github.com/TicketsBot/GoPanel/config"
	dbclient "github.com/TicketsBot/GoPanel/database"
	"github.com/TicketsBot/GoPanel/s3"
	"github.com/TicketsBot/GoPanel/utils"
	"github.com/TicketsBot/common/permission"
	"github.com/gin-gonic/gin"
)

//	func ImportHandler(ctx *gin.Context) {
//		ctx.JSON(401, "This endpoint is disabled")
//	}

func PresignURL(ctx *gin.Context) {
	guildId, userId := ctx.Keys["guildid"].(uint64), ctx.Keys["userid"].(uint64)

	file_type := ctx.Query("file_type")

	bucketName := ""

	if file_type == "data" {
		bucketName = config.Conf.S3Import.DataBucket
	}

	if file_type == "transcripts" {
		bucketName = config.Conf.S3Import.TranscriptBucket
	}

	if bucketName == "" {
		ctx.JSON(400, utils.ErrorStr("Invalid file type"))
		return
	}

	// Get "file_size" query parameter
	fileSize, err := strconv.ParseInt(ctx.Query("file_size"), 10, 64)
	if err != nil {
		ctx.JSON(400, utils.ErrorJson(err))
		return
	}

	fileContentType := ctx.Query("file_content_type")

	if fileContentType == "" {
		ctx.JSON(400, utils.ErrorStr("Missing file_content_type"))
		return
	}

	if fileContentType != "application/zip" && fileContentType != "application/x-zip-compressed" {
		ctx.JSON(400, utils.ErrorStr("Invalid file_content_type"))
		return
	}

	// Check if file is over 1GB
	if fileSize > 1024*1024*1024 {
		ctx.JSON(400, utils.ErrorStr("File size too large"))
		return
	}

	botCtx, err := botcontext.ContextForGuild(guildId)
	if err != nil {
		ctx.JSON(500, utils.ErrorJson(err))
		return
	}

	guild, err := botCtx.GetGuild(context.Background(), guildId)
	if err != nil {
		ctx.JSON(500, utils.ErrorJson(err))
		return
	}

	if guild.OwnerId != userId {
		ctx.JSON(403, utils.ErrorStr("Only the server owner can import transcripts"))
		return
	}

	// Presign URL
	url, err := s3.S3Client.PresignHeader(ctx, "PUT", bucketName, fmt.Sprintf("%s/%d.zip", file_type, guildId), time.Minute*10, url.Values{}, http.Header{
		"Content-Type": []string{fileContentType},
	})
	if err != nil {
		ctx.JSON(500, utils.ErrorJson(err))
		return
	}

	ctx.JSON(200, gin.H{
		"url": url.String(),
	})
}

func GetRuns(ctx *gin.Context) {
	guildId, userId := ctx.Keys["guildid"].(uint64), ctx.Keys["userid"].(uint64)

	permissionLevel, err := utils.GetPermissionLevel(ctx, guildId, userId)
	if err != nil {
		_ = ctx.AbortWithError(http.StatusInternalServerError, app.NewServerError(err))
		return
	}

	if permissionLevel < permission.Admin {
		ctx.JSON(403, utils.ErrorStr("You do not have permission to view import logs"))
		return
	}

	runs, err := dbclient.Client2.ImportLogs.GetRuns(ctx, guildId)
	if err != nil {
		ctx.JSON(500, utils.ErrorJson(err))
		return
	}

	if len(runs) == 0 {
		ctx.JSON(200, []interface{}{})
		return
	}

	ctx.JSON(200, runs)
}

func ImportHandler(ctx *gin.Context) {
	ctx.JSON(401, "Imports are currently disabled - Please try again later (~24 hours)")
}
