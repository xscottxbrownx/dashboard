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
	"time"

	database2 "github.com/TicketsBot-cloud/database"
	"github.com/TicketsBot/GoPanel/app/http/endpoints/api/export/validator"
	"github.com/TicketsBot/GoPanel/botcontext"
	"github.com/TicketsBot/GoPanel/config"
	dbclient "github.com/TicketsBot/GoPanel/database"
	"github.com/TicketsBot/GoPanel/rpc"
	"github.com/TicketsBot/GoPanel/s3"
	"github.com/TicketsBot/GoPanel/utils"
	"github.com/TicketsBot/common/premium"
	"github.com/TicketsBot/database"
	"github.com/gin-gonic/gin"
	"github.com/minio/minio-go/v7"
	"golang.org/x/sync/errgroup"
)

//	func ImportHandler(ctx *gin.Context) {
//		ctx.JSON(401, "This endpoint is disabled")
//	}
func ImportHandler(ctx *gin.Context) {
	// Parse request body from multipart form
	queryCtx, cancel := context.WithTimeout(context.Background(), time.Minute*1500)
	defer cancel()

	var transcriptOutput *validator.GuildTranscriptsOutput
	var data *validator.GuildData

	dataFile, _, dataErr := ctx.Request.FormFile("data_file")
	dataFileExists := dataErr == nil

	transcriptsFile, _, transcriptsErr := ctx.Request.FormFile("transcripts_file")
	transcriptFileExists := transcriptsErr == nil

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

	guildId, selfId := ctx.Keys["guildid"].(uint64), ctx.Keys["userid"].(uint64)

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

		// Upload transcripts
		if transcriptFileExists {
			if _, err := s3.S3Client.PutObject(ctx, config.Conf.S3Import.Bucket, fmt.Sprintf("transcripts/%d.zip", guildId), transcriptReader, transcriptReader.Size(), minio.PutObjectOptions{}); err != nil {
				ctx.JSON(500, utils.ErrorStr("Failed to upload transcripts"))
				return
			}
		}
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

	if guild.OwnerId != selfId {
		ctx.JSON(403, utils.ErrorStr("Only the server owner import server data"))
		return
	}

	if dataFileExists && data.GuildId != guildId || transcriptFileExists && transcriptOutput.GuildId != guildId {
		ctx.JSON(400, utils.ErrorStr("Invalid guild Id"))
		return
	}

	premiumTier, err := rpc.PremiumClient.GetTierByGuildId(queryCtx, guildId, true, botCtx.Token, botCtx.RateLimiter)
	if err != nil {
		ctx.JSON(500, utils.ErrorJson(err))
		return
	}

	// Get ticket maps
	mapping, err := dbclient.Client2.ImportMappingTable.GetMapping(queryCtx, guildId)
	if err != nil {
		ctx.JSON(500, utils.ErrorJson(err))
		return
	}

	var (
		ticketIdMap    = mapping["ticket"]
		formIdMap      = mapping["form"]
		formInputIdMap = mapping["form_input"]
		panelIdMap     = mapping["panel"]
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

	if panelIdMap == nil {
		panelIdMap = make(map[int]int)
	}

	if dataFileExists {

		group, _ := errgroup.WithContext(queryCtx)

		// Import active language
		group.Go(func() (err error) {
			lang := "en"
			if data.ActiveLanguage != nil {
				lang = *data.ActiveLanguage
			}
			_ = dbclient.Client.ActiveLanguage.Set(queryCtx, guildId, lang)

			return
		})

		// Import archive channel
		group.Go(func() (err error) {
			if data.ArchiveChannel != nil {
				err = dbclient.Client.ArchiveChannel.Set(queryCtx, guildId, data.ArchiveChannel)
			}

			return
		})

		// import AutocloseSettings
		group.Go(func() (err error) {
			if data.AutocloseSettings != nil {
				if premiumTier < premium.Premium {
					data.AutocloseSettings.Enabled = false
				}
				err = dbclient.Client.AutoClose.Set(queryCtx, guildId, *data.AutocloseSettings)
			}

			return
		})

		// Import blacklisted users
		group.Go(func() (err error) {
			for _, user := range data.GuildBlacklistedUsers {
				err = dbclient.Client.Blacklist.Add(queryCtx, guildId, user)
				if err != nil {
					return
				}
			}

			return
		})

		// Import channel category
		group.Go(func() (err error) {
			if data.ChannelCategory != nil {
				err = dbclient.Client.ChannelCategory.Set(queryCtx, guildId, *data.ChannelCategory)
			}

			return
		})

		// Import claim settings
		group.Go(func() (err error) {
			if data.ClaimSettings != nil {
				err = dbclient.Client.ClaimSettings.Set(queryCtx, guildId, *data.ClaimSettings)
			}

			return
		})

		// Import close confirmation enabled
		group.Go(func() (err error) {
			err = dbclient.Client.CloseConfirmation.Set(queryCtx, guildId, data.CloseConfirmationEnabled)

			return
		})

		// Import custom colours
		group.Go(func() (err error) {
			if premiumTier < premium.Premium {
				return
			}

			for k, v := range data.CustomColors {
				err = dbclient.Client.CustomColours.Set(queryCtx, guildId, k, v)
				if err != nil {
					return
				}
			}

			return
		})

		// Import feedback enabled
		group.Go(func() (err error) {
			err = dbclient.Client.FeedbackEnabled.Set(queryCtx, guildId, data.FeedbackEnabled)

			return
		})

		// Import is globally blacklisted
		group.Go(func() (err error) {
			if data.GuildIsGloballyBlacklisted {
				reason := "Blacklisted on v1"
				err = dbclient.Client.ServerBlacklist.Add(queryCtx, guildId, &reason)
			}
			return
		})

		// Import Guild Metadata
		group.Go(func() (err error) {
			err = dbclient.Client.GuildMetadata.Set(queryCtx, guildId, data.GuildMetadata)

			return
		})

		// Import Naming Scheme
		group.Go(func() (err error) {
			if data.NamingScheme != nil {
				err = dbclient.Client.NamingScheme.Set(queryCtx, guildId, *data.NamingScheme)
			}

			return
		})

		// Import On Call Users
		group.Go(func() (err error) {
			for _, user := range data.OnCallUsers {
				if isOnCall, oncallerr := dbclient.Client.OnCall.IsOnCall(queryCtx, guildId, user); oncallerr != nil {
					return oncallerr
				} else if !isOnCall {
					_, err = dbclient.Client.OnCall.Toggle(queryCtx, guildId, user)
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
					err = dbclient.Client.Permissions.AddSupport(queryCtx, guildId, perm.Snowflake)
				}

				if perm.IsAdmin {
					err = dbclient.Client.Permissions.AddAdmin(queryCtx, guildId, perm.Snowflake)
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
				err = dbclient.Client.RoleBlacklist.Add(queryCtx, guildId, role)
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
					err = dbclient.Client.RolePermissions.AddSupport(queryCtx, guildId, perm.Snowflake)
				}

				if perm.IsAdmin {
					err = dbclient.Client.RolePermissions.AddAdmin(queryCtx, guildId, perm.Snowflake)
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
				err = dbclient.Client.Tag.Set(queryCtx, tag)
				if err != nil {
					return
				}
			}

			return
		})

		// Import Ticket Limit
		group.Go(func() (err error) {
			if data.TicketLimit != nil {
				err = dbclient.Client.TicketLimit.Set(queryCtx, guildId, uint8(*data.TicketLimit))
			}

			return
		})

		// Import Ticket Permissions
		group.Go(func() (err error) {
			err = dbclient.Client.TicketPermissions.Set(queryCtx, guildId, data.TicketPermissions)

			return
		})

		// Import Users Can Close
		group.Go(func() (err error) {
			err = dbclient.Client.UsersCanClose.Set(queryCtx, guildId, data.UsersCanClose)

			return
		})

		// Import Welcome Message
		group.Go(func() (err error) {
			if data.WelcomeMessage != nil {
				err = dbclient.Client.WelcomeMessages.Set(queryCtx, guildId, *data.WelcomeMessage)
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
			teamId, err := dbclient.Client.SupportTeam.Create(queryCtx, guildId, team.Name)
			if err != nil {
				ctx.JSON(500, utils.ErrorJson(err))
				return
			}

			supportTeamIdMap[team.Id] = teamId
		}

		// Import Support Team Users
		for teamId, users := range data.SupportTeamUsers {
			for _, user := range users {
				if err := dbclient.Client.SupportTeamMembers.Add(queryCtx, supportTeamIdMap[teamId], user); err != nil {
					ctx.JSON(500, utils.ErrorJson(err))
					return
				}
			}
		}

		// Import Support Team Roles
		for teamId, roles := range data.SupportTeamRoles {
			for _, role := range roles {
				if err := dbclient.Client.SupportTeamRoles.Add(queryCtx, supportTeamIdMap[teamId], role); err != nil {
					ctx.JSON(500, utils.ErrorJson(err))
					return
				}
			}
		}

		// Import forms
		for _, form := range data.Forms {
			if _, ok := formIdMap[form.Id]; !ok {
				formId, err := dbclient.Client.Forms.Create(queryCtx, guildId, form.Title, form.CustomId)
				if err != nil {
					return
				}

				formIdMap[form.Id] = formId
			}
		}

		// Import form inputs
		for _, input := range data.FormInputs {
			if _, ok := formInputIdMap[input.Id]; !ok {
				newInputId, err := dbclient.Client.FormInput.Create(queryCtx, formIdMap[input.FormId], input.CustomId, input.Style, input.Label, input.Placeholder, input.Required, input.MinLength, input.MaxLength)
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

			embedId, err := dbclient.Client.Embeds.CreateWithFields(queryCtx, &embed, embedFields)
			if err != nil {
				ctx.JSON(500, utils.ErrorJson(err))
				return
			}

			embedMap[embed.Id] = embedId
		}

		// Panel id map
		existingPanels, err := dbclient.Client.Panel.GetByGuild(queryCtx, guildId)
		if err != nil {
			ctx.JSON(500, utils.ErrorJson(err))
			return
		}

		panelTx, err := dbclient.Client.Panel.Begin(queryCtx)
		if err != nil {
			ctx.JSON(500, utils.ErrorJson(err))
			return
		}
		panelCount := len(existingPanels)

		for _, panel := range data.Panels {
			if _, ok := panelIdMap[panel.PanelId]; ok {
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

				// TODO: Fix this permanently
				panel.MessageId = panel.MessageId - 1

				panelId, err := dbclient.Client.Panel.CreateWithTx(queryCtx, panelTx, panel)
				if err != nil {
					fmt.Println(err)
					ctx.JSON(500, utils.ErrorJson(err))
					return
				}

				panelIdMap[panel.PanelId] = panelId

				panelCount++
			}
		}

		// Import Panel Access Control Rules
		for panelId, rules := range data.PanelAccessControlRules {
			if err := dbclient.Client.PanelAccessControlRules.ReplaceWithTx(queryCtx, panelTx, panelIdMap[panelId], rules); err != nil {
				ctx.JSON(500, utils.ErrorJson(err))
				return
			}
		}

		// Import Panel Mention User
		for panelId, shouldMention := range data.PanelMentionUser {
			if err := dbclient.Client.PanelUserMention.SetWithTx(queryCtx, panelTx, panelIdMap[panelId], shouldMention); err != nil {
				ctx.JSON(500, utils.ErrorJson(err))
				return
			}
		}

		// Import Panel Role Mentions
		for panelId, roles := range data.PanelRoleMentions {
			if err := dbclient.Client.PanelRoleMentions.ReplaceWithTx(queryCtx, panelTx, panelIdMap[panelId], roles); err != nil {
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

			if err := dbclient.Client.PanelTeams.ReplaceWithTx(queryCtx, panelTx, panelIdMap[panelId], teamsToAdd); err != nil {
				ctx.JSON(500, utils.ErrorJson(err))
				return
			}
		}

		if err := panelTx.Commit(queryCtx); err != nil {
			ctx.JSON(500, utils.ErrorJson(err))
			return
		}

		// Import Multi panels
		multiPanelIdMap := make(map[int]int)

		for _, multiPanel := range data.MultiPanels {
			multiPanelId, err := dbclient.Client.MultiPanels.Create(queryCtx, multiPanel)
			if err != nil {
				ctx.JSON(500, utils.ErrorJson(err))
				return
			}

			multiPanelIdMap[multiPanel.Id] = multiPanelId
		}

		// Import Multi Panel Targets
		for multiPanelId, panelIds := range data.MultiPanelTargets {
			for _, panelId := range panelIds {
				if err := dbclient.Client.MultiPanelTargets.Insert(queryCtx, multiPanelIdMap[multiPanelId], panelIdMap[panelId]); err != nil {
					ctx.JSON(500, utils.ErrorJson(err))
					return
				}
			}
		}

		if data.Settings.ContextMenuPanel != nil {
			newContextMenuPanel := panelIdMap[*data.Settings.ContextMenuPanel]
			data.Settings.ContextMenuPanel = &newContextMenuPanel
		}

		if err := dbclient.Client.Settings.Set(queryCtx, guildId, data.Settings); err != nil {
			ctx.JSON(500, utils.ErrorJson(err))
			return
		}

		ticketCount, err := dbclient.Client.Tickets.GetTotalTicketCount(queryCtx, guildId)
		if err != nil {
			ctx.JSON(500, utils.ErrorJson(err))
			return
		}
		ticketsToCreate := make([]database2.Ticket, len(data.Tickets))
		ticketIdMap = make(map[int]int)

		for i, ticket := range data.Tickets {
			if _, ok := ticketIdMap[ticket.Id]; !ok {
				var panelId *int
				if ticket.PanelId != nil {
					a := panelIdMap[*ticket.PanelId]
					panelId = &a
				}
				ticketsToCreate[i] = database2.Ticket{
					Id:               ticket.Id + ticketCount,
					GuildId:          guildId,
					ChannelId:        ticket.ChannelId,
					UserId:           ticket.UserId,
					Open:             ticket.Open,
					OpenTime:         ticket.OpenTime,
					WelcomeMessageId: ticket.WelcomeMessageId,
					PanelId:          panelId,
					HasTranscript:    ticket.HasTranscript,
					CloseTime:        ticket.CloseTime,
					IsThread:         ticket.IsThread,
					JoinMessageId:    ticket.JoinMessageId,
					NotesThreadId:    ticket.NotesThreadId,
				}

				ticketIdMap[ticket.Id] = ticket.Id + ticketCount
			}
		}

		if err := dbclient.Client2.Tickets.BulkImport(queryCtx, guildId, ticketsToCreate); err != nil {
			ctx.JSON(500, utils.ErrorJson(err))
			return
		}

		ticketsExtrasGroup, _ := errgroup.WithContext(queryCtx)

		ticketsExtrasGroup.Go(func() (err error) {
			newMembersMap := make(map[int][]uint64)
			for ticketId, members := range data.TicketAdditionalMembers {
				newMembersMap[ticketIdMap[ticketId]] = members
			}
			// Remap ticket ids in data.TicketAdditionalMembers
			err = dbclient.Client2.TicketMembers.ImportBulk(queryCtx, guildId, newMembersMap)
			return
		})

		// Import ticket last messages
		ticketsExtrasGroup.Go(func() (err error) {
			for _, msg := range data.TicketLastMessages {
				lastMessageId := uint64(0)
				if msg.Data.LastMessageId != nil {
					lastMessageId = *msg.Data.LastMessageId
				}

				userId := uint64(0)
				if msg.Data.UserId != nil {
					userId = *msg.Data.UserId
				}

				userIsStaff := false
				if msg.Data.UserIsStaff != nil {
					userIsStaff = *msg.Data.UserIsStaff
				}

				err = dbclient.Client.TicketLastMessage.Set(queryCtx, guildId, ticketIdMap[msg.TicketId], lastMessageId, userId, userIsStaff)
			}
			return
		})

		ticketsExtrasGroup.Go(func() (err error) {
			newClaimsMap := make(map[int]uint64)
			for ticketId, user := range data.TicketClaims {
				newClaimsMap[ticketIdMap[ticketId]] = user.Data
			}

			err = dbclient.Client2.TicketClaims.ImportBulk(queryCtx, guildId, newClaimsMap)
			return
		})

		// Import ticket ratings
		ticketsExtrasGroup.Go(func() (err error) {
			newRatingsMap := make(map[int]uint8)
			for ticketId, rating := range data.ServiceRatings {
				newRatingsMap[ticketIdMap[ticketId]] = uint8(rating.Data)
			}

			err = dbclient.Client2.ServiceRatings.ImportBulk(queryCtx, guildId, newRatingsMap)
			return
		})

		// Import participants
		ticketsExtrasGroup.Go(func() (err error) {
			newParticipantsMap := make(map[int][]uint64)
			for ticketId, participants := range data.Participants {
				newParticipantsMap[ticketIdMap[ticketId]] = participants
			}

			err = dbclient.Client2.Participants.ImportBulk(queryCtx, guildId, newParticipantsMap)
			return
		})

		// Import First Response Times
		ticketsExtrasGroup.Go(func() (err error) {
			for _, frt := range data.FirstResponseTimes {
				err = dbclient.Client.FirstResponseTime.Set(queryCtx, guildId, frt.UserId, ticketIdMap[frt.TicketId], frt.ResponseTime)
			}
			return
		})

		ticketsExtrasGroup.Go(func() (err error) {
			for _, response := range data.ExitSurveyResponses {

				resps := map[int]string{
					*response.Data.QuestionId: *response.Data.Response,
				}

				err = dbclient.Client.ExitSurveyResponses.AddResponses(queryCtx, guildId, ticketIdMap[response.TicketId], formIdMap[*response.Data.FormId], resps)
			}
			return
		})

		// Import Close Reasons
		ticketsExtrasGroup.Go(func() (err error) {
			for _, reason := range data.CloseReasons {
				err = dbclient.Client.CloseReason.Set(queryCtx, guildId, ticketIdMap[reason.TicketId], reason.Data)
			}
			return
		})

		// Import Autoclose Excluded Tickets
		ticketsExtrasGroup.Go(func() (err error) {
			for _, ticketId := range data.AutocloseExcluded {
				err = dbclient.Client.AutoCloseExclude.Exclude(queryCtx, guildId, ticketIdMap[ticketId])
			}
			return
		})

		// Import Archive Messages
		ticketsExtrasGroup.Go(func() (err error) {
			for _, message := range data.ArchiveMessages {
				err = dbclient.Client.ArchiveMessages.Set(queryCtx, guildId, ticketIdMap[message.TicketId], message.Data.ChannelId, message.Data.MessageId)
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
	newMapping["panel"] = panelIdMap

	for area, m := range newMapping {
		for sourceId, targetId := range m {
			if err := dbclient.Client2.ImportMappingTable.Set(queryCtx, guildId, area, sourceId, targetId); err != nil {
				ctx.JSON(500, utils.ErrorJson(err))
				return
			}
		}
	}

	ctx.JSON(200, utils.SuccessResponse)
}
