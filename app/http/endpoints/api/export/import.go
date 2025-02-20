package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/TicketsBot/GoPanel/botcontext"
	"github.com/TicketsBot/GoPanel/config"
	"github.com/TicketsBot/GoPanel/s3"
	"github.com/TicketsBot/GoPanel/utils"
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
	url, err := s3.S3Client.PresignHeader(ctx, "PUT", bucketName, fmt.Sprintf("%s/%d.zip", file_type, guildId), time.Minute*1, url.Values{}, http.Header{
		"Content-Type": []string{"application/x-zip-compressed"},
	})
	if err != nil {
		ctx.JSON(500, utils.ErrorJson(err))
		return
	}

	ctx.JSON(200, gin.H{
		"url": url.String(),
	})
}

func ImportHandler(ctx *gin.Context) {
	ctx.JSON(401, "Imports are currently disabled - Please try again later (~24 hours)")
}
