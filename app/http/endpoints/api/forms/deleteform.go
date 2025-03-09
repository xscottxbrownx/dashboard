package forms

import (
	"net/http"
	"strconv"

	"github.com/TicketsBot-cloud/dashboard/app"
	dbclient "github.com/TicketsBot-cloud/dashboard/database"
	"github.com/TicketsBot-cloud/dashboard/utils"
	"github.com/gin-gonic/gin"
)

func DeleteForm(c *gin.Context) {
	guildId := c.Keys["guildid"].(uint64)

	formId, err := strconv.Atoi(c.Param("form_id"))
	if err != nil {
		c.JSON(400, utils.ErrorStr("Invalid form ID"))
		return
	}

	form, ok, err := dbclient.Client.Forms.Get(c, formId)
	if err != nil {
		_ = c.AbortWithError(http.StatusInternalServerError, app.NewServerError(err))
		return
	}

	if !ok {
		c.JSON(404, utils.ErrorStr("Form not found"))
		return
	}

	if form.GuildId != guildId {
		c.JSON(403, utils.ErrorStr("Form does not belong to this guild"))
		return
	}

	if err := dbclient.Client.Forms.Delete(c, formId); err != nil {
		_ = c.AbortWithError(http.StatusInternalServerError, app.NewServerError(err))
		return
	}

	c.JSON(200, utils.SuccessResponse)
}
