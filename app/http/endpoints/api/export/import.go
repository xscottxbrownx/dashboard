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
	"github.com/TicketsBot/GoPanel/log"
	"github.com/TicketsBot/GoPanel/rpc"
	"github.com/TicketsBot/GoPanel/s3"
	"github.com/TicketsBot/GoPanel/utils"
	"github.com/TicketsBot/common/premium"
	"github.com/TicketsBot/database"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v4"
	"github.com/minio/minio-go/v7"
	"go.uber.org/zap"
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
		validator.WithMaxUncompressedSize(500*1024*1024),
		validator.WithMaxIndividualFileSize(100*1024*1024),
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
			log.Logger.Info("Imported active language", zap.Uint64("guild", guildId), zap.String("language", lang))

			return
		})

		// Import archive channel
		group.Go(func() (err error) {
			if data.ArchiveChannel != nil {
				err = dbclient.Client.ArchiveChannel.Set(queryCtx, guildId, data.ArchiveChannel)
				log.Logger.Info("Imported archive channel", zap.Uint64("guild", guildId), zap.Uint64("channel", *data.ArchiveChannel))
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
				log.Logger.Info("Imported autoclose settings", zap.Uint64("guild", guildId), zap.Bool("enabled", data.AutocloseSettings.Enabled))
			}

			return
		})

		// Import blacklisted users
		group.Go(func() (err error) {
			for _, user := range data.GuildBlacklistedUsers {
				err = dbclient.Client.Blacklist.Add(queryCtx, guildId, user)
				log.Logger.Info("Imported blacklisted user", zap.Uint64("guild", guildId), zap.Uint64("user", user))
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
				log.Logger.Info("Imported channel category", zap.Uint64("guild", guildId), zap.Uint64("category", *data.ChannelCategory))
			}

			return
		})

		// Import claim settings
		group.Go(func() (err error) {
			if data.ClaimSettings != nil {
				err = dbclient.Client.ClaimSettings.Set(queryCtx, guildId, *data.ClaimSettings)
				log.Logger.Info("Imported claim settings", zap.Uint64("guild", guildId), zap.Any("settings", data.ClaimSettings))
			}

			return
		})

		// Import close confirmation enabled
		group.Go(func() (err error) {
			err = dbclient.Client.CloseConfirmation.Set(queryCtx, guildId, data.CloseConfirmationEnabled)
			log.Logger.Info("Imported close confirmation enabled", zap.Uint64("guild", guildId), zap.Bool("enabled", data.CloseConfirmationEnabled))
			return
		})

		// Import custom colours
		group.Go(func() (err error) {
			if premiumTier < premium.Premium {
				return
			}

			for k, v := range data.CustomColors {
				err = dbclient.Client.CustomColours.Set(queryCtx, guildId, k, v)
				log.Logger.Info("Imported custom colour", zap.Uint64("guild", guildId), zap.Int16("key", k), zap.Int("value", v))
				if err != nil {
					return
				}
			}

			return
		})

		// Import feedback enabled
		group.Go(func() (err error) {
			err = dbclient.Client.FeedbackEnabled.Set(queryCtx, guildId, data.FeedbackEnabled)
			log.Logger.Info("Imported feedback enabled", zap.Uint64("guild", guildId), zap.Bool("enabled", data.FeedbackEnabled))
			return
		})

		// Import is globally blacklisted
		group.Go(func() (err error) {
			if data.GuildIsGloballyBlacklisted {
				reason := "Blacklisted on v1"
				err = dbclient.Client.ServerBlacklist.Add(queryCtx, guildId, &reason)
				log.Logger.Info("Imported globally blacklisted", zap.Uint64("guild", guildId))
			}
			return
		})

		// Import Guild Metadata
		group.Go(func() (err error) {
			err = dbclient.Client.GuildMetadata.Set(queryCtx, guildId, data.GuildMetadata)
			log.Logger.Info("Imported guild metadata", zap.Uint64("guild", guildId), zap.Any("metadata", data.GuildMetadata))
			return
		})

		// Import Naming Scheme
		group.Go(func() (err error) {
			if data.NamingScheme != nil {
				err = dbclient.Client.NamingScheme.Set(queryCtx, guildId, *data.NamingScheme)
				log.Logger.Info("Imported naming scheme", zap.Uint64("guild", guildId), zap.Any("scheme", data.NamingScheme))
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
					log.Logger.Info("Imported user permission", zap.Uint64("guild", guildId), zap.Uint64("user", perm.Snowflake), zap.Bool("support", true))
				}

				if perm.IsAdmin {
					err = dbclient.Client.Permissions.AddAdmin(queryCtx, guildId, perm.Snowflake)
					log.Logger.Info("Imported user permission", zap.Uint64("guild", guildId), zap.Uint64("user", perm.Snowflake), zap.Bool("admin", true))
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
				log.Logger.Info("Imported guild blacklisted role", zap.Uint64("guild", guildId), zap.Uint64("role", role))
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
					log.Logger.Info("Imported role permission", zap.Uint64("guild", guildId), zap.Uint64("role", perm.Snowflake), zap.Bool("support", true))
				}

				if perm.IsAdmin {
					err = dbclient.Client.RolePermissions.AddAdmin(queryCtx, guildId, perm.Snowflake)
					log.Logger.Info("Imported role permission", zap.Uint64("guild", guildId), zap.Uint64("role", perm.Snowflake), zap.Bool("admin", true))
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
				log.Logger.Info("Imported tag", zap.Uint64("guild", guildId), zap.String("name", tag.Id))
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
				log.Logger.Info("Imported ticket limit", zap.Uint64("guild", guildId), zap.Uint8("limit", uint8(*data.TicketLimit)))
			}

			return
		})

		// Import Ticket Permissions
		group.Go(func() (err error) {
			err = dbclient.Client.TicketPermissions.Set(queryCtx, guildId, data.TicketPermissions)
			log.Logger.Info("Imported ticket permissions", zap.Uint64("guild", guildId), zap.Any("permissions", data.TicketPermissions))

			return
		})

		// Import Users Can Close
		group.Go(func() (err error) {
			err = dbclient.Client.UsersCanClose.Set(queryCtx, guildId, data.UsersCanClose)
			log.Logger.Info("Imported users can close", zap.Uint64("guild", guildId), zap.Bool("can_close", data.UsersCanClose))

			return
		})

		// Import Welcome Message
		group.Go(func() (err error) {
			if data.WelcomeMessage != nil {
				err = dbclient.Client.WelcomeMessages.Set(queryCtx, guildId, *data.WelcomeMessage)
				log.Logger.Info("Imported welcome message", zap.Uint64("guild", guildId), zap.String("message", *data.WelcomeMessage))
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
			teamId, err := dbclient.Client.SupportTeam.Create(queryCtx, guildId, fmt.Sprintf("%s (Imported)", team.Name))
			if err != nil {
				ctx.JSON(500, utils.ErrorJson(err))
				return
			}
			log.Logger.Info("Imported support team", zap.Uint64("guild", guildId), zap.String("name", team.Name))

			supportTeamIdMap[team.Id] = teamId
		}

		// Import Support Team Users
		log.Logger.Info("Importing support team users", zap.Uint64("guild", guildId))
		for teamId, users := range data.SupportTeamUsers {
			for _, user := range users {
				_ = dbclient.Client.SupportTeamMembers.Add(queryCtx, supportTeamIdMap[teamId], user)
			}
		}

		// Import Support Team Roles
		log.Logger.Info("Importing support team roles", zap.Uint64("guild", guildId))
		for teamId, roles := range data.SupportTeamRoles {
			for _, role := range roles {
				_ = dbclient.Client.SupportTeamRoles.Add(queryCtx, supportTeamIdMap[teamId], role)
			}
		}

		// Import forms
		log.Logger.Info("Importing forms", zap.Uint64("guild", guildId))
		for _, form := range data.Forms {
			if _, ok := formIdMap[form.Id]; !ok {
				newCustomId, _ := utils.RandString(30)
				formId, err := dbclient.Client.Forms.Create(queryCtx, guildId, fmt.Sprintf("%s (Imported)", form.Title), newCustomId)
				log.Logger.Info("Imported form", zap.Uint64("guild", guildId), zap.String("title", form.Title))
				if err != nil {
					return
				}

				formIdMap[form.Id] = formId
			}
		}

		log.Logger.Info("Importing mapping for forms", zap.Uint64("guild", guildId))
		for area, m := range map[string]map[int]int{"form": formIdMap} {
			for sourceId, targetId := range m {
				if err := dbclient.Client2.ImportMappingTable.Set(queryCtx, guildId, area, sourceId, targetId); err != nil {
					ctx.JSON(500, utils.ErrorJson(err))
					return
				}
			}
		}

		// Import form inputs
		log.Logger.Info("Importing form inputs", zap.Uint64("guild", guildId))
		for _, input := range data.FormInputs {
			if _, ok := formInputIdMap[input.Id]; !ok {
				newCustomId, _ := utils.RandString(30)
				newInputId, err := dbclient.Client.FormInput.Create(queryCtx, formIdMap[input.FormId], newCustomId, input.Style, input.Label, input.Placeholder, input.Required, input.MinLength, input.MaxLength)
				if err != nil {
					ctx.JSON(500, utils.ErrorJson(err))
					return
				}

				formInputIdMap[input.Id] = newInputId
			}
		}

		log.Logger.Info("Importing mapping for forms inputs", zap.Uint64("guild", guildId))
		for area, m := range map[string]map[int]int{"form_input": formInputIdMap} {
			for sourceId, targetId := range m {
				if err := dbclient.Client2.ImportMappingTable.Set(queryCtx, guildId, area, sourceId, targetId); err != nil {
					ctx.JSON(500, utils.ErrorJson(err))
					return
				}
			}
		}

		embedMap := make(map[int]int)

		// Import embeds
		log.Logger.Info("Importing embeds", zap.Uint64("guild", guildId))
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
		panelCount := len(existingPanels)

		panelTx, _ := dbclient.Client.Panel.BeginTx(queryCtx, pgx.TxOptions{})

		// Import Panels
		log.Logger.Info("Importing panels", zap.Uint64("guild", guildId))
		for _, panel := range data.Panels {
			if _, ok := panelIdMap[panel.PanelId]; !ok {
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

				panel.Title = fmt.Sprintf("%s (Imported)", panel.Title)

				// TODO: Fix this permanently
				panel.MessageId = panel.MessageId - 1
				newCustomId, _ := utils.RandString(30)
				panel.CustomId = newCustomId

				panelId, err := dbclient.Client.Panel.CreateWithTx(queryCtx, panelTx, panel)
				if err != nil {
					fmt.Println(err)
					ctx.JSON(500, utils.ErrorJson(err))
					return
				}
				log.Logger.Info("Imported panel", zap.Uint64("guild", guildId), zap.Int("panel", panel.PanelId), zap.Int("new_panel", panelId))

				panelIdMap[panel.PanelId] = panelId

				panelCount++
			}
		}

		if err := panelTx.Commit(queryCtx); err != nil {
			ctx.JSON(500, utils.ErrorJson(err))
			return
		}

		log.Logger.Info("Importing mapping for panels", zap.Uint64("guild", guildId))
		for area, m := range map[string]map[int]int{"panel": panelIdMap} {
			for sourceId, targetId := range m {
				if err := dbclient.Client2.ImportMappingTable.Set(queryCtx, guildId, area, sourceId, targetId); err != nil {
					ctx.JSON(500, utils.ErrorJson(err))
					return
				}
			}
		}

		// Import Panel Access Control Rules
		log.Logger.Info("Importing panel access control rules", zap.Uint64("guild", guildId))
		for panelId, rules := range data.PanelAccessControlRules {
			if err := dbclient.Client.PanelAccessControlRules.Replace(queryCtx, panelIdMap[panelId], rules); err != nil {
				ctx.JSON(500, utils.ErrorJson(err))
				return
			}
		}

		// Import Panel Mention User
		log.Logger.Info("Importing panel mention user", zap.Uint64("guild", guildId))
		for panelId, shouldMention := range data.PanelMentionUser {
			if err := dbclient.Client.PanelUserMention.Set(queryCtx, panelIdMap[panelId], shouldMention); err != nil {
				ctx.JSON(500, utils.ErrorJson(err))
				return
			}
		}

		// Import Panel Role Mentions
		log.Logger.Info("Importing panel role mentions", zap.Uint64("guild", guildId))
		for panelId, roles := range data.PanelRoleMentions {
			if err := dbclient.Client.PanelRoleMentions.Replace(queryCtx, panelIdMap[panelId], roles); err != nil {
				ctx.JSON(500, utils.ErrorJson(err))
				return
			}
		}

		// Import Panel Teams
		log.Logger.Info("Importing panel teams", zap.Uint64("guild", guildId))
		for panelId, teams := range data.PanelTeams {
			teamsToAdd := make([]int, len(teams))
			for _, team := range teams {
				teamsToAdd = append(teamsToAdd, supportTeamIdMap[team])
			}

			if err := dbclient.Client.PanelTeams.Replace(queryCtx, panelIdMap[panelId], teamsToAdd); err != nil {
				ctx.JSON(500, utils.ErrorJson(err))
				return
			}
		}

		// Import Multi panels
		log.Logger.Info("Importing multi panels", zap.Uint64("guild", guildId))
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
		log.Logger.Info("Importing multi panel targets", zap.Uint64("guild", guildId))
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

		// Import settings
		log.Logger.Info("Importing settings", zap.Uint64("guild", guildId))
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

		// Import tickets
		for i, ticket := range data.Tickets {
			if _, ok := ticketIdMap[ticket.Id]; !ok {
				log.Logger.Info("Importing ticket", zap.Uint64("guild", guildId), zap.Int("ticket", ticket.Id), zap.Int("new_ticket", ticket.Id+ticketCount))
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

		log.Logger.Info("Importing tickets", zap.Uint64("guild", guildId))
		if err := dbclient.Client2.Tickets.BulkImport(queryCtx, guildId, ticketsToCreate); err != nil {
			ctx.JSON(500, utils.ErrorJson(err))
			return
		}

		// Update the mapping
		log.Logger.Info("Importing mapping for tickets", zap.Uint64("guild", guildId))
		for area, m := range map[string]map[int]int{"ticket": ticketIdMap} {
			for sourceId, targetId := range m {
				if err := dbclient.Client2.ImportMappingTable.Set(queryCtx, guildId, area, sourceId, targetId); err != nil {
					ctx.JSON(500, utils.ErrorJson(err))
					return
				}
			}
		}

		ticketsExtrasGroup, _ := errgroup.WithContext(queryCtx)

		ticketsExtrasGroup.Go(func() (err error) {
			log.Logger.Info("Importing ticket messages", zap.Uint64("guild", guildId))
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
			log.Logger.Info("Importing ticket last messages", zap.Uint64("guild", guildId))
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
			log.Logger.Info("Importing ticket claims", zap.Uint64("guild", guildId))
			newClaimsMap := make(map[int]uint64)
			for ticketId, user := range data.TicketClaims {
				newClaimsMap[ticketIdMap[ticketId]] = user.Data
			}

			err = dbclient.Client2.TicketClaims.ImportBulk(queryCtx, guildId, newClaimsMap)
			return
		})

		// Import ticket ratings
		ticketsExtrasGroup.Go(func() (err error) {
			log.Logger.Info("Importing ticket ratings", zap.Uint64("guild", guildId))
			newRatingsMap := make(map[int]uint8)
			for ticketId, rating := range data.ServiceRatings {
				newRatingsMap[ticketIdMap[ticketId]] = uint8(rating.Data)
			}

			err = dbclient.Client2.ServiceRatings.ImportBulk(queryCtx, guildId, newRatingsMap)
			return
		})

		// Import participants
		ticketsExtrasGroup.Go(func() (err error) {
			log.Logger.Info("Importing ticket participants", zap.Uint64("guild", guildId))
			newParticipantsMap := make(map[int][]uint64)
			for ticketId, participants := range data.Participants {
				newParticipantsMap[ticketIdMap[ticketId]] = participants
			}

			err = dbclient.Client2.Participants.ImportBulk(queryCtx, guildId, newParticipantsMap)
			return
		})

		// Import First Response Times
		ticketsExtrasGroup.Go(func() (err error) {
			log.Logger.Info("Importing first response times", zap.Uint64("guild", guildId))
			for _, frt := range data.FirstResponseTimes {
				err = dbclient.Client.FirstResponseTime.Set(queryCtx, guildId, frt.UserId, ticketIdMap[frt.TicketId], frt.ResponseTime)
			}
			return
		})

		ticketsExtrasGroup.Go(func() (err error) {
			log.Logger.Info("Importing ticket notes", zap.Uint64("guild", guildId))
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
			log.Logger.Info("Importing close reasons", zap.Uint64("guild", guildId))
			for _, reason := range data.CloseReasons {
				err = dbclient.Client.CloseReason.Set(queryCtx, guildId, ticketIdMap[reason.TicketId], reason.Data)
			}
			return
		})

		// Import Autoclose Excluded Tickets
		ticketsExtrasGroup.Go(func() (err error) {
			log.Logger.Info("Importing autoclose excluded tickets", zap.Uint64("guild", guildId))
			for _, ticketId := range data.AutocloseExcluded {
				err = dbclient.Client.AutoCloseExclude.Exclude(queryCtx, guildId, ticketIdMap[ticketId])
			}
			return
		})

		// Import Archive Messages
		ticketsExtrasGroup.Go(func() (err error) {
			log.Logger.Info("Importing archive messages", zap.Uint64("guild", guildId))
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

	ctx.JSON(200, utils.SuccessResponse)
}
