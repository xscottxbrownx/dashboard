package forms

import (
	"net/http"

	"github.com/TicketsBot-cloud/dashboard/app"
	dbclient "github.com/TicketsBot-cloud/dashboard/database"
	"github.com/TicketsBot-cloud/database"
	"github.com/gin-gonic/gin"
)

type embeddedForm struct {
	database.Form
	Inputs []database.FormInput `json:"inputs"`
}

func GetForms(c *gin.Context) {
	guildId := c.Keys["guildid"].(uint64)

	forms, err := dbclient.Client.Forms.GetForms(c, guildId)
	if err != nil {
		_ = c.AbortWithError(http.StatusInternalServerError, app.NewServerError(err))
		return
	}

	inputs, err := dbclient.Client.FormInput.GetInputsForGuild(c, guildId)
	if err != nil {
		_ = c.AbortWithError(http.StatusInternalServerError, app.NewServerError(err))
		return
	}

	data := make([]embeddedForm, len(forms))
	for i, form := range forms {
		formInputs, ok := inputs[form.Id]
		if !ok {
			formInputs = make([]database.FormInput, 0)
		}

		data[i] = embeddedForm{
			Form:   form,
			Inputs: formInputs,
		}
	}

	c.JSON(200, data)
}
