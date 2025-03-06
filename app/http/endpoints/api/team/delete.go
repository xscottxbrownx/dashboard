package api

import (
	"strconv"

	dbclient "github.com/TicketsBot-cloud/dashboard/database"
	"github.com/TicketsBot-cloud/dashboard/utils"
	"github.com/gin-gonic/gin"
)

func DeleteTeam(ctx *gin.Context) {
	guildId := ctx.Keys["guildid"].(uint64)

	teamId, err := strconv.Atoi(ctx.Param("teamid"))
	if err != nil {
		ctx.JSON(400, utils.ErrorJson(err))
		return
	}

	// check team belongs to guild
	exists, err := dbclient.Client.SupportTeam.Exists(ctx, teamId, guildId)
	if err != nil {
		ctx.JSON(500, utils.ErrorJson(err))
		return
	}

	if !exists {
		ctx.JSON(400, utils.ErrorStr("Team not found"))
		return
	}

	if err := dbclient.Client.SupportTeam.Delete(ctx, teamId); err != nil {
		ctx.JSON(500, utils.ErrorJson(err))
		return
	}

	ctx.JSON(200, utils.SuccessResponse)
}
