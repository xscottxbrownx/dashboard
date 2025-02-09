package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"os"

	"github.com/TicketsBot/GoPanel/app/http/endpoints/api/export/validator"
	"github.com/TicketsBot/GoPanel/botcontext"
	dbclient "github.com/TicketsBot/GoPanel/database"
	"github.com/TicketsBot/GoPanel/rpc"
	"github.com/TicketsBot/GoPanel/utils"
	"github.com/TicketsBot/common/premium"
	"github.com/TicketsBot/database"
	"github.com/gin-gonic/gin"
	"golang.org/x/sync/errgroup"
)

func ImportHandler(ctx *gin.Context) {
	// Parse request body from multipart form

	var transcriptOutput *validator.GuildTranscriptsOutput
	var data *validator.GuildData

	dataFile, _, err := ctx.Request.FormFile("data_file")
	dataFileExists := err == nil

	transcriptsFile, _, err := ctx.Request.FormFile("transcripts_file")
	transcriptFileExists := err == nil

	// Decrypt file
	publicKeyBlock, _ := pem.Decode([]byte(os.Getenv("V1_PUBLIC_KEY")))
	if publicKeyBlock == nil {
		ctx.JSON(400, utils.ErrorStr("Invalid public key"))
		return
	}

	parsedKey, err := x509.ParsePKIXPublicKey(publicKeyBlock.Bytes)
	if err != nil {
		ctx.JSON(400, utils.ErrorJson(err))
		return
	}

	decryptedPublicKey, ok := parsedKey.(ed25519.PublicKey)
	if !ok {
		ctx.JSON(400, utils.ErrorStr("Invalid public key"))
		return
	}

	v := validator.NewValidator(
		decryptedPublicKey,
		validator.WithMaxUncompressedSize(250*1024*1024),
		validator.WithMaxIndividualFileSize(1*1024*1024),
	)

	if dataFileExists {
		defer dataFile.Close()

		dataBuffer := bytes.NewBuffer(nil)
		if _, err := io.Copy(dataBuffer, dataFile); err != nil {
			ctx.JSON(500, utils.ErrorJson(err))
			return
		}

		dataReader := bytes.NewReader(dataBuffer.Bytes())
		data, err = v.ValidateGuildData(dataReader, dataReader.Size())
		if err != nil {
			ctx.JSON(400, utils.ErrorJson(err))
			return
		}
	}

	if transcriptFileExists {
		defer transcriptsFile.Close()

		transcriptsBuffer := bytes.NewBuffer(nil)
		if _, err := io.Copy(transcriptsBuffer, transcriptsFile); err != nil {
			ctx.JSON(500, utils.ErrorJson(err))
			return
		}

		transcriptReader := bytes.NewReader(transcriptsBuffer.Bytes())
		transcriptOutput, err = v.ValidateGuildTranscripts(transcriptReader, transcriptReader.Size())
		if err != nil {
			ctx.JSON(400, utils.ErrorJson(err))
			return
		}
	}

	guildId, selfId := ctx.Keys["guildid"].(uint64), ctx.Keys["userid"].(uint64)

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

	if guild.OwnerId != selfId {
		ctx.JSON(403, utils.ErrorStr("Only the server owner import server data"))
		return
	}

	if dataFileExists && data.GuildId != guildId || transcriptFileExists && transcriptOutput.GuildId != guildId {
		ctx.JSON(400, utils.ErrorStr("Invalid guild Id"))
		return
	}

	premiumTier, err := rpc.PremiumClient.GetTierByGuildId(ctx, guildId, true, botCtx.Token, botCtx.RateLimiter)
	if err != nil {
		ctx.JSON(500, utils.ErrorJson(err))
		return
	}

	// Get ticket maps
	mapping, err := dbclient.Client2.ImportMappingTable.GetMapping(ctx, guildId)
	if err != nil {
		ctx.JSON(500, utils.ErrorJson(err))
		return
	}

	var (
		ticketIdMap    = mapping["ticket"]
		formIdMap      = mapping["form"]
		formInputIdMap = mapping["form_input"]
	)

	if ticketIdMap == nil {
		ticketIdMap = make(map[int]int)
	}

	if formIdMap == nil {
		formIdMap = make(map[int]int)
	}

	if formInputIdMap == nil {
		formInputIdMap = make(map[int]int)
	}

	if dataFileExists {

		group, _ := errgroup.WithContext(ctx)

		// Import active language
		group.Go(func() (err error) {
			lang := "en"
			if data.ActiveLanguage != nil {
				lang = *data.ActiveLanguage
			}
			_ = dbclient.Client.ActiveLanguage.Set(ctx, guildId, lang)

			return
		})

		// Import archive channel
		group.Go(func() (err error) {
			if data.ArchiveChannel != nil {
				err = dbclient.Client.ArchiveChannel.Set(ctx, guildId, data.ArchiveChannel)
			}

			return
		})

		// import AutocloseSettings
		group.Go(func() (err error) {
			if data.AutocloseSettings != nil {
				if premiumTier < premium.Premium {
					data.AutocloseSettings.Enabled = false
				}
				err = dbclient.Client.AutoClose.Set(ctx, guildId, *data.AutocloseSettings)
			}

			return
		})

		// Import blacklisted users
		group.Go(func() (err error) {
			for _, user := range data.GuildBlacklistedUsers {
				err = dbclient.Client.Blacklist.Add(ctx, guildId, user)
				if err != nil {
					return
				}
			}

			return
		})

		// Import channel category
		group.Go(func() (err error) {
			if data.ChannelCategory != nil {
				err = dbclient.Client.ChannelCategory.Set(ctx, guildId, *data.ChannelCategory)
			}

			return
		})

		// Import claim settings
		group.Go(func() (err error) {
			if data.ClaimSettings != nil {
				err = dbclient.Client.ClaimSettings.Set(ctx, guildId, *data.ClaimSettings)
			}

			return
		})

		// Import close confirmation enabled
		group.Go(func() (err error) {
			err = dbclient.Client.CloseConfirmation.Set(ctx, guildId, data.CloseConfirmationEnabled)

			return
		})

		// Import custom colours
		group.Go(func() (err error) {
			if premiumTier < premium.Premium {
				return
			}

			for k, v := range data.CustomColors {
				err = dbclient.Client.CustomColours.Set(ctx, guildId, k, v)
				if err != nil {
					return
				}
			}

			return
		})

		// Import feedback enabled
		group.Go(func() (err error) {
			err = dbclient.Client.FeedbackEnabled.Set(ctx, guildId, data.FeedbackEnabled)

			return
		})

		// Import is globally blacklisted
		group.Go(func() (err error) {
			if data.GuildIsGloballyBlacklisted {
				reason := "Blacklisted on v1"
				err = dbclient.Client.ServerBlacklist.Add(ctx, guildId, &reason)
			}
			return
		})

		// Import Guild Metadata
		group.Go(func() (err error) {
			err = dbclient.Client.GuildMetadata.Set(ctx, guildId, data.GuildMetadata)

			return
		})

		// Import Naming Scheme
		group.Go(func() (err error) {
			if data.NamingScheme != nil {
				err = dbclient.Client.NamingScheme.Set(ctx, guildId, *data.NamingScheme)
			}

			return
		})

		// Import On Call Users
		group.Go(func() (err error) {
			for _, user := range data.OnCallUsers {
				if isOnCall, oncallerr := dbclient.Client.OnCall.IsOnCall(ctx, guildId, user); oncallerr != nil {
					return oncallerr
				} else if !isOnCall {
					_, err = dbclient.Client.OnCall.Toggle(ctx, guildId, user)
					if err != nil {
						return
					}
				}
			}

			return
		})

		// Import User Permissions
		group.Go(func() (err error) {
			for _, perm := range data.UserPermissions {
				if perm.IsSupport {
					err = dbclient.Client.Permissions.AddSupport(ctx, guildId, perm.Snowflake)
				}

				if perm.IsAdmin {
					err = dbclient.Client.Permissions.AddAdmin(ctx, guildId, perm.Snowflake)
				}

				if err != nil {
					return
				}
			}

			return
		})

		// Import Guild Blacklisted Roles
		group.Go(func() (err error) {
			for _, role := range data.GuildBlacklistedRoles {
				err = dbclient.Client.RoleBlacklist.Add(ctx, guildId, role)
				if err != nil {
					return
				}
			}

			return
		})

		// Import Role Permissions
		group.Go(func() (err error) {
			for _, perm := range data.RolePermissions {
				if perm.IsSupport {
					err = dbclient.Client.RolePermissions.AddSupport(ctx, guildId, perm.Snowflake)
				}

				if perm.IsAdmin {
					err = dbclient.Client.RolePermissions.AddAdmin(ctx, guildId, perm.Snowflake)
				}

				if err != nil {
					return
				}
			}

			return
		})

		// Import Tags
		group.Go(func() (err error) {
			for _, tag := range data.Tags {
				err = dbclient.Client.Tag.Set(ctx, tag)
				if err != nil {
					return
				}
			}

			return
		})

		// Import Ticket Limit
		group.Go(func() (err error) {
			if data.TicketLimit != nil {
				err = dbclient.Client.TicketLimit.Set(ctx, guildId, uint8(*data.TicketLimit))
			}

			return
		})

		// Import Ticket Permissions
		group.Go(func() (err error) {
			err = dbclient.Client.TicketPermissions.Set(ctx, guildId, data.TicketPermissions)

			return
		})

		// Import Users Can Close
		group.Go(func() (err error) {
			err = dbclient.Client.UsersCanClose.Set(ctx, guildId, data.UsersCanClose)

			return
		})

		// Import Welcome Message
		group.Go(func() (err error) {
			if data.WelcomeMessage != nil {
				err = dbclient.Client.WelcomeMessages.Set(ctx, guildId, *data.WelcomeMessage)
			}

			return
		})

		if err := group.Wait(); err != nil {
			ctx.JSON(500, utils.ErrorJson(err))
			return
		}

		supportTeamIdMap := make(map[int]int)

		// Import Support Teams
		for _, team := range data.SupportTeams {
			teamId, err := dbclient.Client.SupportTeam.Create(ctx, guildId, team.Name)
			if err != nil {
				ctx.JSON(500, utils.ErrorJson(err))
				return
			}

			supportTeamIdMap[team.Id] = teamId
		}

		// Import Support Team Users
		for teamId, users := range data.SupportTeamUsers {
			for _, user := range users {
				if err := dbclient.Client.SupportTeamMembers.Add(ctx, supportTeamIdMap[teamId], user); err != nil {
					ctx.JSON(500, utils.ErrorJson(err))
					return
				}
			}
		}

		// Import Support Team Roles
		for teamId, roles := range data.SupportTeamRoles {
			for _, role := range roles {
				if err := dbclient.Client.SupportTeamRoles.Add(ctx, supportTeamIdMap[teamId], role); err != nil {
					ctx.JSON(500, utils.ErrorJson(err))
					return
				}
			}
		}

		// Import forms
		for _, form := range data.Forms {
			if _, ok := formIdMap[form.Id]; !ok {
				fmt.Println("Creating form", form.Title)
				formId, err := dbclient.Client.Forms.Create(ctx, guildId, form.Title, form.CustomId)
				if err != nil {
					return
				}

				fmt.Println("Form ID", form.Id, "New ID", formId)

				formIdMap[form.Id] = formId
			}
		}

		// Import form inputs
		for _, input := range data.FormInputs {
			if _, ok := formInputIdMap[input.Id]; !ok {
				newInputId, err := dbclient.Client.FormInput.Create(ctx, formIdMap[input.FormId], input.CustomId, input.Style, input.Label, input.Placeholder, input.Required, input.MinLength, input.MaxLength)
				if err != nil {
					ctx.JSON(500, utils.ErrorJson(err))
					return
				}

				formInputIdMap[input.Id] = newInputId
			}
		}

		embedMap := make(map[int]int)

		// Import embeds
		for _, embed := range data.Embeds {
			var embedFields []database.EmbedField

			for _, field := range data.EmbedFields {
				if field.EmbedId == embed.Id {
					embedFields = append(embedFields, field)
				}
			}

			embed.GuildId = guildId

			embedId, err := dbclient.Client.Embeds.CreateWithFields(ctx, &embed, embedFields)
			if err != nil {
				ctx.JSON(500, utils.ErrorJson(err))
				return
			}

			fmt.Println("Embed ID", embed.Id, "New ID", embedId)

			embedMap[embed.Id] = embedId
		}

		// Panel id map
		existingPanels, err := dbclient.Client.Panel.GetByGuild(ctx, guildId)
		if err != nil {
			ctx.JSON(500, utils.ErrorJson(err))
			return
		}

		panelTx, err := dbclient.Client.Panel.Begin(ctx)
		if err != nil {
			ctx.JSON(500, utils.ErrorJson(err))
			return
		}
		panelCount := len(existingPanels)

		panelIdMap := make(map[int]int)

		for _, panel := range data.Panels {
			if premiumTier < premium.Premium && panelCount > 2 {
				panel.ForceDisabled = true
				panel.Disabled = true
			}

			if panel.FormId != nil {
				newFormId := formIdMap[*panel.FormId]
				panel.FormId = &newFormId
			}

			if panel.ExitSurveyFormId != nil {
				newFormId := formIdMap[*panel.ExitSurveyFormId]
				panel.ExitSurveyFormId = &newFormId
			}

			if panel.WelcomeMessageEmbed != nil {
				newEmbedId := embedMap[*panel.WelcomeMessageEmbed]
				panel.WelcomeMessageEmbed = &newEmbedId
			}

			panelId, err := dbclient.Client.Panel.CreateWithTx(ctx, panelTx, panel)
			if err != nil {
				ctx.JSON(500, utils.ErrorJson(err))
				return
			}

			panelIdMap[panel.PanelId] = panelId

			panelCount++
		}

		// Import Panel Access Control Rules
		for panelId, rules := range data.PanelAccessControlRules {
			if err := dbclient.Client.PanelAccessControlRules.ReplaceWithTx(ctx, panelTx, panelIdMap[panelId], rules); err != nil {
				ctx.JSON(500, utils.ErrorJson(err))
				return
			}
		}

		// Import Panel Mention User
		for panelId, shouldMention := range data.PanelMentionUser {
			if err := dbclient.Client.PanelUserMention.SetWithTx(ctx, panelTx, panelIdMap[panelId], shouldMention); err != nil {
				ctx.JSON(500, utils.ErrorJson(err))
				return
			}
		}

		// Import Panel Role Mentions
		for panelId, roles := range data.PanelRoleMentions {
			if err := dbclient.Client.PanelRoleMentions.ReplaceWithTx(ctx, panelTx, panelIdMap[panelId], roles); err != nil {
				ctx.JSON(500, utils.ErrorJson(err))
				return
			}
		}

		// Import Panel Teams
		for panelId, teams := range data.PanelTeams {
			teamsToAdd := make([]int, len(teams))
			for _, team := range teams {
				teamsToAdd = append(teamsToAdd, supportTeamIdMap[team])
			}

			if err := dbclient.Client.PanelTeams.ReplaceWithTx(ctx, panelTx, panelIdMap[panelId], teamsToAdd); err != nil {
				ctx.JSON(500, utils.ErrorJson(err))
				return
			}
		}

		if err := panelTx.Commit(ctx); err != nil {
			ctx.JSON(500, utils.ErrorJson(err))
			return
		}

		// Import Multi panels
		multiPanelIdMap := make(map[int]int)

		for _, multiPanel := range data.MultiPanels {
			multiPanelId, err := dbclient.Client.MultiPanels.Create(ctx, multiPanel)
			if err != nil {
				ctx.JSON(500, utils.ErrorJson(err))
				return
			}

			multiPanelIdMap[multiPanel.Id] = multiPanelId
		}

		// Import Multi Panel Targets
		for multiPanelId, panelIds := range data.MultiPanelTargets {
			for _, panelId := range panelIds {
				if err := dbclient.Client.MultiPanelTargets.Insert(ctx, multiPanelIdMap[multiPanelId], panelIdMap[panelId]); err != nil {
					ctx.JSON(500, utils.ErrorJson(err))
					return
				}
			}
		}

		newContextMenuPanel := panelIdMap[*data.Settings.ContextMenuPanel]
		data.Settings.ContextMenuPanel = &newContextMenuPanel

		if err := dbclient.Client.Settings.Set(ctx, guildId, data.Settings); err != nil {
			ctx.JSON(500, utils.ErrorJson(err))
			return
		}

		// Import tickets
		for _, ticket := range data.Tickets {
			if _, ok := ticketIdMap[ticket.Id]; !ok {
				newPanelId := panelIdMap[*ticket.PanelId]
				newTicketId, err := dbclient.Client.Tickets.Create(ctx, guildId, ticket.UserId, ticket.IsThread, &newPanelId)
				if err != nil {
					ctx.JSON(500, utils.ErrorJson(err))
					return
				}

				ticketIdMap[ticket.Id] = newTicketId

				if ticket.Open {
					if err := dbclient.Client.Tickets.SetOpen(ctx, guildId, newTicketId); err != nil {
						ctx.JSON(500, utils.ErrorJson(err))
						return
					}
				} else {
					if err := dbclient.Client.Tickets.Close(ctx, newTicketId, guildId); err != nil {
						ctx.JSON(500, utils.ErrorJson(err))
						return
					}
				}

				if err := dbclient.Client.Tickets.SetChannelId(ctx, guildId, newTicketId, *ticket.ChannelId); err != nil {
					ctx.JSON(500, utils.ErrorJson(err))
					return
				}
				if err := dbclient.Client.Tickets.SetHasTranscript(ctx, guildId, newTicketId, ticket.HasTranscript); err != nil {
					ctx.JSON(500, utils.ErrorJson(err))
					return
				}
				if ticket.NotesThreadId != nil {
					if err := dbclient.Client.Tickets.SetNotesThreadId(ctx, guildId, newTicketId, *ticket.NotesThreadId); err != nil {
						ctx.JSON(500, utils.ErrorJson(err))
						return
					}
				}
				if err := dbclient.Client.Tickets.SetStatus(ctx, guildId, newTicketId, ticket.Status); err != nil {
					ctx.JSON(500, utils.ErrorJson(err))
					return
				}
			}
		}
	}

	// Upload transcripts
	if transcriptFileExists {
		for ticketId, transcript := range transcriptOutput.Transcripts {
			if err := utils.ArchiverClient.ImportTranscript(ctx, guildId, ticketIdMap[ticketId], transcript); err != nil {
				ctx.JSON(500, utils.ErrorJson(err))
				return
			}
		}
	}

	if dataFileExists {

		ticketsExtrasGroup, _ := errgroup.WithContext(ctx)

		ticketsExtrasGroup.Go(func() (err error) {
			// Import ticket additional members
			for ticketId, members := range data.TicketAdditionalMembers {
				for _, member := range members {
					err = dbclient.Client.TicketMembers.Add(ctx, guildId, ticketIdMap[ticketId], member)
					return
				}
			}
			return
		})

		// Import ticket last messages
		ticketsExtrasGroup.Go(func() (err error) {
			for _, msg := range data.TicketLastMessages {
				err = dbclient.Client.TicketLastMessage.Set(ctx, guildId, ticketIdMap[msg.TicketId], *msg.Data.LastMessageId, *msg.Data.UserId, *msg.Data.UserIsStaff)
			}
			return
		})

		// Import ticket claims
		ticketsExtrasGroup.Go(func() (err error) {
			for _, claim := range data.TicketClaims {
				err = dbclient.Client.TicketClaims.Set(ctx, guildId, ticketIdMap[claim.TicketId], claim.Data)
			}
			return
		})

		// Import ticket ratings
		ticketsExtrasGroup.Go(func() (err error) {
			for _, rating := range data.ServiceRatings {
				err = dbclient.Client.ServiceRatings.Set(ctx, guildId, ticketIdMap[rating.TicketId], uint8(rating.Data))
			}
			return
		})

		// Import participants
		ticketsExtrasGroup.Go(func() (err error) {
			for ticketId, participants := range data.Participants {
				err = dbclient.Client.Participants.SetBulk(ctx, guildId, ticketIdMap[ticketId], participants)
			}
			return
		})

		// Import First Response Times
		ticketsExtrasGroup.Go(func() (err error) {
			for _, frt := range data.FirstResponseTimes {
				err = dbclient.Client.FirstResponseTime.Set(ctx, guildId, frt.UserId, ticketIdMap[frt.TicketId], frt.ResponseTime)
			}
			return
		})

		ticketsExtrasGroup.Go(func() (err error) {
			for _, response := range data.ExitSurveyResponses {

				resps := map[int]string{
					*response.Data.QuestionId: *response.Data.Response,
				}

				err = dbclient.Client.ExitSurveyResponses.AddResponses(ctx, guildId, ticketIdMap[response.TicketId], formIdMap[*response.Data.FormId], resps)
			}
			return
		})

		// Import Close Reasons
		ticketsExtrasGroup.Go(func() (err error) {
			for _, reason := range data.CloseReasons {
				err = dbclient.Client.CloseReason.Set(ctx, guildId, ticketIdMap[reason.TicketId], reason.Data)
			}
			return
		})

		// Import Autoclose Excluded Tickets
		ticketsExtrasGroup.Go(func() (err error) {
			for _, ticketId := range data.AutocloseExcluded {
				err = dbclient.Client.AutoCloseExclude.Exclude(ctx, guildId, ticketIdMap[ticketId])
			}
			return
		})

		// Import Archive Messages
		ticketsExtrasGroup.Go(func() (err error) {
			for _, message := range data.ArchiveMessages {
				err = dbclient.Client.ArchiveMessages.Set(ctx, guildId, ticketIdMap[message.TicketId], message.Data.ChannelId, message.Data.MessageId)
			}
			return
		})

		if err := ticketsExtrasGroup.Wait(); err != nil {
			ctx.JSON(500, utils.ErrorJson(err))
			return
		}

	}

	// Imported successfully, update the import mapping
	newMapping := make(map[string]map[int]int)
	newMapping["ticket"] = ticketIdMap
	newMapping["form"] = formIdMap
	newMapping["form_input"] = formInputIdMap

	for area, m := range newMapping {
		for sourceId, targetId := range m {
			if err := dbclient.Client2.ImportMappingTable.Set(ctx, guildId, area, sourceId, targetId); err != nil {
				ctx.JSON(500, utils.ErrorJson(err))
				return
			}
		}
	}

	ctx.JSON(200, utils.SuccessResponse)
}
