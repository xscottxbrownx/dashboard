package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
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

func PresignTranscriptURL(ctx *gin.Context) {
	guildId, userId := ctx.Keys["guildid"].(uint64), ctx.Keys["userid"].(uint64)

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
	url, err := s3.S3Client.PresignHeader(ctx, "PUT", config.Conf.S3Import.Bucket, fmt.Sprintf("transcripts/%d.zip", guildId), time.Minute*1, url.Values{}, http.Header{
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
	// Return slices
	successfulItems := make([]string, 0)
	failedItems := make([]string, 0)
	skippedItems := make([]string, 0)

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
				failedItems = append(failedItems, "Transcripts")
			} else {
				successfulItems = append(successfulItems, "Transcripts")
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

		if data.GuildIsGloballyBlacklisted {
			reason := "Blacklisted on v1"
			_ = dbclient.Client.ServerBlacklist.Add(queryCtx, guildId, &reason)
			log.Logger.Info("Imported globally blacklisted", zap.Uint64("guild", guildId))

			ctx.JSON(403, "This server is blacklisted on v1")
			return
		}

		group, _ := errgroup.WithContext(queryCtx)

		// Import active language
		group.Go(func() (err error) {
			lang := "en"
			if data.ActiveLanguage != nil {
				lang = *data.ActiveLanguage
			}
			if err := dbclient.Client.ActiveLanguage.Set(queryCtx, guildId, lang); err != nil {
				failedItems = append(failedItems, "Language")
			} else {
				successfulItems = append(successfulItems, "Language")
			}
			log.Logger.Info("Imported active language", zap.Uint64("guild", guildId), zap.String("language", lang))

			return
		})

		// Import archive channel
		group.Go(func() (err error) {
			if data.ArchiveChannel != nil {
				if err := dbclient.Client.ArchiveChannel.Set(queryCtx, guildId, data.ArchiveChannel); err != nil {
					failedItems = append(failedItems, "Archive Channel")
				} else {
					successfulItems = append(successfulItems, "Archive Channel")
				}
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
				if err := dbclient.Client.AutoClose.Set(queryCtx, guildId, *data.AutocloseSettings); err != nil {
					failedItems = append(failedItems, "Autoclose Settings")
				} else {
					successfulItems = append(successfulItems, "Autoclose Settings")
				}
				log.Logger.Info("Imported autoclose settings", zap.Uint64("guild", guildId), zap.Bool("enabled", data.AutocloseSettings.Enabled))
			}

			return
		})

		// Import blacklisted users
		group.Go(func() (err error) {
			failedUsers := 0
			for _, user := range data.GuildBlacklistedUsers {
				err = dbclient.Client.Blacklist.Add(queryCtx, guildId, user)
				log.Logger.Info("Imported blacklisted user", zap.Uint64("guild", guildId), zap.Uint64("user", user))
				if err != nil {
					failedUsers++
				}
			}

			if failedUsers == 0 {
				successfulItems = append(successfulItems, "Blacklisted Users")
			} else {
				failedItems = append(failedItems, fmt.Sprintf("Blacklisted Users (x%d)", failedUsers))
			}

			return
		})

		// Import channel category
		group.Go(func() (err error) {
			if data.ChannelCategory != nil {
				if err := dbclient.Client.ChannelCategory.Set(queryCtx, guildId, *data.ChannelCategory); err != nil {
					failedItems = append(failedItems, "Channel Category")
				} else {
					successfulItems = append(successfulItems, "Channel Category")
				}
				log.Logger.Info("Imported channel category", zap.Uint64("guild", guildId), zap.Uint64("category", *data.ChannelCategory))
			}

			return
		})

		// Import claim settings
		group.Go(func() (err error) {
			if data.ClaimSettings != nil {
				if err := dbclient.Client.ClaimSettings.Set(queryCtx, guildId, *data.ClaimSettings); err != nil {
					failedItems = append(failedItems, "Claim Settings")
				} else {
					successfulItems = append(successfulItems, "Claim Settings")
				}
				log.Logger.Info("Imported claim settings", zap.Uint64("guild", guildId), zap.Any("settings", data.ClaimSettings))
			}

			return
		})

		// Import close confirmation enabled
		group.Go(func() (err error) {
			if err := dbclient.Client.CloseConfirmation.Set(queryCtx, guildId, data.CloseConfirmationEnabled); err != nil {
				failedItems = append(failedItems, "Close Confirmation")
			} else {
				successfulItems = append(successfulItems, "Close Confirmation")
			}
			log.Logger.Info("Imported close confirmation enabled", zap.Uint64("guild", guildId), zap.Bool("enabled", data.CloseConfirmationEnabled))
			return
		})

		// Import custom colours
		group.Go(func() (err error) {
			if premiumTier < premium.Premium {
				return
			}

			failedColours := 0

			for k, v := range data.CustomColors {
				err = dbclient.Client.CustomColours.Set(queryCtx, guildId, k, v)
				log.Logger.Info("Imported custom colour", zap.Uint64("guild", guildId), zap.Int16("key", k), zap.Int("value", v))
				if err != nil {
					failedColours++
				}
			}

			if failedColours == 0 {
				successfulItems = append(successfulItems, "Custom Colours")
			} else {
				failedItems = append(failedItems, fmt.Sprintf("Custom Colours (x%d)", failedColours))
			}

			return
		})

		// Import feedback enabled
		group.Go(func() (err error) {
			if err := dbclient.Client.FeedbackEnabled.Set(queryCtx, guildId, data.FeedbackEnabled); err != nil {
				failedItems = append(failedItems, "Feedback Enabled")
			} else {
				successfulItems = append(successfulItems, "Feedback Enabled")
			}
			log.Logger.Info("Imported feedback enabled", zap.Uint64("guild", guildId), zap.Bool("enabled", data.FeedbackEnabled))
			return
		})

		// Import Guild Metadata
		group.Go(func() (err error) {
			if err := dbclient.Client.GuildMetadata.Set(queryCtx, guildId, data.GuildMetadata); err != nil {
				failedItems = append(failedItems, "Guild Metadata")
			} else {
				successfulItems = append(successfulItems, "Guild Metadata")
			}
			log.Logger.Info("Imported guild metadata", zap.Uint64("guild", guildId), zap.Any("metadata", data.GuildMetadata))
			return
		})

		// Import Naming Scheme
		group.Go(func() (err error) {
			if data.NamingScheme != nil {
				if err := dbclient.Client.NamingScheme.Set(queryCtx, guildId, *data.NamingScheme); err != nil {
					failedItems = append(failedItems, "Naming Scheme")
				} else {
					successfulItems = append(successfulItems, "Naming Scheme")
				}
				log.Logger.Info("Imported naming scheme", zap.Uint64("guild", guildId), zap.Any("scheme", data.NamingScheme))
			}

			return
		})

		// Import On Call Users
		group.Go(func() (err error) {

			failedUsers := 0

			for _, user := range data.OnCallUsers {
				if isOnCall, oncallerr := dbclient.Client.OnCall.IsOnCall(queryCtx, guildId, user); oncallerr != nil {
					return oncallerr
				} else if !isOnCall {
					if _, err := dbclient.Client.OnCall.Toggle(queryCtx, guildId, user); err != nil {
						failedUsers++
					}
				}
			}

			if failedUsers == 0 {
				successfulItems = append(successfulItems, "On Call Users")
			} else {
				failedItems = append(failedItems, fmt.Sprintf("On Call Users (x%d)", failedUsers))
			}

			return
		})

		// Import User Permissions
		group.Go(func() (err error) {

			failedUsers := 0

			for _, perm := range data.UserPermissions {
				if perm.IsSupport {
					if err := dbclient.Client.Permissions.AddSupport(queryCtx, guildId, perm.Snowflake); err != nil {
						failedUsers++
					}
					log.Logger.Info("Imported user permission", zap.Uint64("guild", guildId), zap.Uint64("user", perm.Snowflake), zap.Bool("support", true))
				}

				if perm.IsAdmin {
					if err := dbclient.Client.Permissions.AddAdmin(queryCtx, guildId, perm.Snowflake); err != nil {
						failedUsers++
					}
					log.Logger.Info("Imported user permission", zap.Uint64("guild", guildId), zap.Uint64("user", perm.Snowflake), zap.Bool("admin", true))
				}
			}

			if failedUsers == 0 {
				successfulItems = append(successfulItems, "User Permissions")
			} else {
				failedItems = append(failedItems, fmt.Sprintf("User Permissions (x%d)", failedUsers))
			}

			return
		})

		// Import Guild Blacklisted Roles
		group.Go(func() (err error) {

			failedRoles := 0

			for _, role := range data.GuildBlacklistedRoles {
				if err := dbclient.Client.RoleBlacklist.Add(queryCtx, guildId, role); err != nil {
					failedRoles++
				}
				log.Logger.Info("Imported guild blacklisted role", zap.Uint64("guild", guildId), zap.Uint64("role", role))
			}

			if failedRoles == 0 {
				successfulItems = append(successfulItems, "Guild Blacklisted Roles")
			} else {
				failedItems = append(failedItems, fmt.Sprintf("Guild Blacklisted Roles (x%d)", failedRoles))
			}

			return
		})

		// Import Role Permissions
		group.Go(func() (err error) {

			failedRoles := 0

			for _, perm := range data.RolePermissions {
				if perm.IsSupport {
					if err := dbclient.Client.RolePermissions.AddSupport(queryCtx, guildId, perm.Snowflake); err != nil {
						failedRoles++
					}
					log.Logger.Info("Imported role permission", zap.Uint64("guild", guildId), zap.Uint64("role", perm.Snowflake), zap.Bool("support", true))
				}

				if perm.IsAdmin {
					if err := dbclient.Client.RolePermissions.AddAdmin(queryCtx, guildId, perm.Snowflake); err != nil {
						failedRoles++
					}
					log.Logger.Info("Imported role permission", zap.Uint64("guild", guildId), zap.Uint64("role", perm.Snowflake), zap.Bool("admin", true))
				}
			}

			if failedRoles == 0 {
				successfulItems = append(successfulItems, "Role Permissions")
			} else {
				failedItems = append(failedItems, fmt.Sprintf("Role Permissions (x%d)", failedRoles))
			}

			return
		})

		// Import Tags
		group.Go(func() (err error) {

			failedTags := 0

			for _, tag := range data.Tags {
				if err := dbclient.Client.Tag.Set(queryCtx, tag); err != nil {
					failedTags++
				}
				log.Logger.Info("Imported tag", zap.Uint64("guild", guildId), zap.String("name", tag.Id))
			}

			if failedTags == 0 {
				successfulItems = append(successfulItems, "Tags")
			} else {
				failedItems = append(failedItems, fmt.Sprintf("Tags (x%d)", failedTags))
			}

			return
		})

		// Import Ticket Limit
		group.Go(func() (err error) {
			if data.TicketLimit != nil {
				if err := dbclient.Client.TicketLimit.Set(queryCtx, guildId, uint8(*data.TicketLimit)); err != nil {
					failedItems = append(failedItems, "Ticket Limit")
				} else {
					successfulItems = append(successfulItems, "Ticket Limit")
				}
				log.Logger.Info("Imported ticket limit", zap.Uint64("guild", guildId), zap.Uint8("limit", uint8(*data.TicketLimit)))
			}

			return
		})

		// Import Ticket Permissions
		group.Go(func() (err error) {
			if err := dbclient.Client.TicketPermissions.Set(queryCtx, guildId, data.TicketPermissions); err != nil {
				failedItems = append(failedItems, "Ticket Permissions")
			} else {
				successfulItems = append(successfulItems, "Ticket Permissions")
			}
			log.Logger.Info("Imported ticket permissions", zap.Uint64("guild", guildId), zap.Any("permissions", data.TicketPermissions))

			return
		})

		// Import Users Can Close
		group.Go(func() (err error) {
			if err := dbclient.Client.UsersCanClose.Set(queryCtx, guildId, data.UsersCanClose); err != nil {
				failedItems = append(failedItems, "Users Can Close")
			} else {
				successfulItems = append(successfulItems, "Users Can Close")
			}
			log.Logger.Info("Imported users can close", zap.Uint64("guild", guildId), zap.Bool("can_close", data.UsersCanClose))

			return
		})

		// Import Welcome Message
		group.Go(func() (err error) {
			if data.WelcomeMessage != nil {
				if err := dbclient.Client.WelcomeMessages.Set(queryCtx, guildId, *data.WelcomeMessage); err != nil {
					failedItems = append(failedItems, "Welcome Message")
				} else {
					successfulItems = append(successfulItems, "Welcome Message")
				}
				log.Logger.Info("Imported welcome message", zap.Uint64("guild", guildId), zap.String("message", *data.WelcomeMessage))
			}

			return
		})

		_ = group.Wait()

		supportTeamIdMap := make(map[int]int)

		failedSupportTeams := 0

		// Import Support Teams
		for _, team := range data.SupportTeams {
			teamId, err := dbclient.Client.SupportTeam.Create(queryCtx, guildId, fmt.Sprintf("%s (Imported)", team.Name))
			if err != nil {
				failedSupportTeams++
			}
			log.Logger.Info("Imported support team", zap.Uint64("guild", guildId), zap.String("name", team.Name))

			supportTeamIdMap[team.Id] = teamId
		}

		if failedSupportTeams == 0 {
			successfulItems = append(successfulItems, "Support Teams")
		} else {
			failedItems = append(failedItems, fmt.Sprintf("Support Teams (x%d)", failedSupportTeams))
		}

		failedSupportTeamUsers := 0

		// Import Support Team Users
		log.Logger.Info("Importing support team users", zap.Uint64("guild", guildId))
		for teamId, users := range data.SupportTeamUsers {
			for _, user := range users {
				if err := dbclient.Client.SupportTeamMembers.Add(queryCtx, supportTeamIdMap[teamId], user); err != nil {
					failedSupportTeamUsers++
				}
			}
		}

		if failedSupportTeamUsers == 0 {
			successfulItems = append(successfulItems, "Support Team Users")
		} else {
			failedItems = append(failedItems, fmt.Sprintf("Support Team Users (x%d)", failedSupportTeamUsers))
		}

		failedSupportTeamRoles := 0

		// Import Support Team Roles
		log.Logger.Info("Importing support team roles", zap.Uint64("guild", guildId))
		for teamId, roles := range data.SupportTeamRoles {
			for _, role := range roles {
				if err := dbclient.Client.SupportTeamRoles.Add(queryCtx, supportTeamIdMap[teamId], role); err != nil {
					failedSupportTeamRoles++
				}
			}
		}

		if failedSupportTeamRoles == 0 {
			successfulItems = append(successfulItems, "Support Team Roles")
		} else {
			failedItems = append(failedItems, fmt.Sprintf("Support Team Roles (x%d)", failedSupportTeamRoles))
		}

		// Import forms
		log.Logger.Info("Importing forms", zap.Uint64("guild", guildId))
		failedForms := make([]int, 0)
		for _, form := range data.Forms {
			if _, ok := formIdMap[form.Id]; !ok {
				newCustomId, _ := utils.RandString(30)
				formId, err := dbclient.Client.Forms.Create(queryCtx, guildId, fmt.Sprintf("%s (Imported)", form.Title), newCustomId)
				log.Logger.Info("Imported form", zap.Uint64("guild", guildId), zap.String("title", form.Title))
				if err != nil {
					failedForms = append(failedForms, form.Id)
				} else {
					formIdMap[form.Id] = formId
				}
			} else {
				skippedItems = append(skippedItems, fmt.Sprintf("Form (Title: %s)", form.Title))
			}
		}

		if len(failedForms) == 0 {
			successfulItems = append(successfulItems, "Forms")
		} else {
			failedItems = append(failedItems, fmt.Sprintf("Forms (x%d)", len(failedForms)))
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
		failedFormInputs := make([]int, 0)
		for _, input := range data.FormInputs {
			if failedForms != nil && utils.Contains(failedForms, input.FormId) {
				skippedItems = append(skippedItems, fmt.Sprintf("Form Input (Form: %d, ID: %d)", input.FormId, input.Id))
				continue
			}
			if _, ok := formInputIdMap[input.Id]; !ok {
				newCustomId, _ := utils.RandString(30)
				newInputId, err := dbclient.Client.FormInput.Create(queryCtx, formIdMap[input.FormId], newCustomId, input.Style, input.Label, input.Placeholder, input.Required, input.MinLength, input.MaxLength)
				if err != nil {
					failedItems = append(failedItems, fmt.Sprintf("Form Input (Form: %d, ID: %d)", input.FormId, input.Id))
					failedFormInputs = append(failedFormInputs, input.Id)
				} else {
					formInputIdMap[input.Id] = newInputId
				}
			} else {
				skippedItems = append(skippedItems, fmt.Sprintf("Form Input (Form: %d, ID: %d)", input.FormId, input.Id))
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
		failedEmbeds := make([]int, 0)
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
				failedEmbeds = append(failedEmbeds, embed.Id)
			} else {
				log.Logger.Info("Imported embed", zap.Uint64("guild", guildId), zap.Int("embed", embed.Id), zap.Int("new_embed", embedId))
				embedMap[embed.Id] = embedId
			}
		}

		if len(failedEmbeds) == 0 {
			successfulItems = append(successfulItems, "Embeds")
		} else {
			failedItems = append(failedItems, fmt.Sprintf("Embeds (x%d)", len(failedEmbeds)))
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
		failedPanels := make([]int, 0)
		for _, panel := range data.Panels {
			if _, ok := panelIdMap[panel.PanelId]; !ok {
				if premiumTier < premium.Premium && panelCount > 2 {
					panel.ForceDisabled = true
					panel.Disabled = true
				}

				if panel.FormId != nil {
					if failedForms != nil && utils.Contains(failedForms, *panel.FormId) {
						skippedItems = append(skippedItems, fmt.Sprintf("Panel (ID: %d, missing form)", panel.PanelId))
						continue
					}
					newFormId := formIdMap[*panel.FormId]
					panel.FormId = &newFormId
				}

				if panel.ExitSurveyFormId != nil {
					newFormId := formIdMap[*panel.ExitSurveyFormId]
					panel.ExitSurveyFormId = &newFormId
				}

				if panel.WelcomeMessageEmbed != nil {
					if failedEmbeds == nil || !utils.Contains(failedEmbeds, *panel.WelcomeMessageEmbed) {
						newEmbedId := embedMap[*panel.WelcomeMessageEmbed]
						panel.WelcomeMessageEmbed = &newEmbedId
					}
				}

				panel.Title = fmt.Sprintf("%s (Imported)", panel.Title)

				// TODO: Fix this permanently
				panel.MessageId = panel.MessageId - 1
				newCustomId, _ := utils.RandString(30)
				panel.CustomId = newCustomId

				panelId, err := dbclient.Client.Panel.CreateWithTx(queryCtx, panelTx, panel)
				if err != nil {
					failedPanels = append(failedPanels, panel.PanelId)
				} else {
					log.Logger.Info("Imported panel", zap.Uint64("guild", guildId), zap.Int("panel", panel.PanelId), zap.Int("new_panel", panelId))
					panelIdMap[panel.PanelId] = panelId
					panelCount++
				}
			}
		}

		if len(failedPanels) == 0 {
			successfulItems = append(successfulItems, "Panels")
		} else {
			failedItems = append(failedItems, fmt.Sprintf("Panels (x%d)", len(failedPanels)))
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
		failedPanelAccessControlRules := 0
		for panelId, rules := range data.PanelAccessControlRules {
			if len(failedPanels) > 0 && utils.Contains(failedPanels, panelId) {
				skippedItems = append(skippedItems, fmt.Sprintf("Panel Access Control Rules (Panel: %d)", panelId))
				continue
			}
			if _, ok := panelIdMap[panelId]; ok {
				if err := dbclient.Client.PanelAccessControlRules.Replace(queryCtx, panelIdMap[panelId], rules); err != nil {
					failedPanelAccessControlRules++
				}
			} else {
				skippedItems = append(skippedItems, fmt.Sprintf("Panel Access Control Rules (Panel: %d)", panelId))
			}
		}

		if failedPanelAccessControlRules == 0 {
			successfulItems = append(successfulItems, "Panel Access Control Rules")
		} else {
			failedItems = append(failedItems, fmt.Sprintf("Panel Access Control Rules (x%d)", failedPanelAccessControlRules))
		}

		// Import Panel Mention User
		log.Logger.Info("Importing panel mention user", zap.Uint64("guild", guildId))
		failedPanelMentionUser := 0
		for panelId, shouldMention := range data.PanelMentionUser {
			if len(failedPanels) > 0 && utils.Contains(failedPanels, panelId) {
				skippedItems = append(skippedItems, fmt.Sprintf("Panel Mention User (Panel: %d)", panelId))
				continue
			}
			if err := dbclient.Client.PanelUserMention.Set(queryCtx, panelIdMap[panelId], shouldMention); err != nil {
				failedPanelMentionUser++
			}
		}

		if failedPanelMentionUser == 0 {
			successfulItems = append(successfulItems, "Panel Mention User")
		} else {
			failedItems = append(failedItems, fmt.Sprintf("Panel Mention User (x%d)", failedPanelMentionUser))
		}

		// Import Panel Role Mentions
		failedPanelRoleMentions := 0
		log.Logger.Info("Importing panel role mentions", zap.Uint64("guild", guildId))
		for panelId, roles := range data.PanelRoleMentions {
			if len(failedPanels) > 0 && utils.Contains(failedPanels, panelId) {
				skippedItems = append(skippedItems, fmt.Sprintf("Panel Role Mentions (Panel: %d)", panelId))
				continue
			}
			if err := dbclient.Client.PanelRoleMentions.Replace(queryCtx, panelIdMap[panelId], roles); err != nil {
				failedPanelRoleMentions++
			}
		}

		if failedPanelRoleMentions == 0 {
			successfulItems = append(successfulItems, "Panel Role Mentions")
		} else {
			failedItems = append(failedItems, fmt.Sprintf("Panel Role Mentions (x%d)", failedPanelRoleMentions))
		}

		// Import Panel Teams
		log.Logger.Info("Importing panel teams", zap.Uint64("guild", guildId))
		failedPanelTeams := 0
		for panelId, teams := range data.PanelTeams {
			if len(failedPanels) > 0 && utils.Contains(failedPanels, panelId) {
				skippedItems = append(skippedItems, fmt.Sprintf("Panel Teams (Panel: %d)", panelId))
				continue
			}
			teamsToAdd := make([]int, len(teams))
			for _, team := range teams {
				teamsToAdd = append(teamsToAdd, supportTeamIdMap[team])
			}

			if err := dbclient.Client.PanelTeams.Replace(queryCtx, panelIdMap[panelId], teamsToAdd); err != nil {
				failedPanelTeams++
			}
		}

		if failedPanelTeams == 0 {
			successfulItems = append(successfulItems, "Panel Teams")
		} else {
			failedItems = append(failedItems, fmt.Sprintf("Panel Teams (x%d)", failedPanelTeams))
		}

		// Import Multi panels
		log.Logger.Info("Importing multi panels", zap.Uint64("guild", guildId))
		multiPanelIdMap := make(map[int]int)
		failedMultiPanels := make([]int, 0)
		for _, multiPanel := range data.MultiPanels {
			multiPanelId, err := dbclient.Client.MultiPanels.Create(queryCtx, multiPanel)
			if err != nil {
				failedMultiPanels = append(failedMultiPanels, multiPanel.Id)
			} else {
				log.Logger.Info("Imported multi panel", zap.Uint64("guild", guildId), zap.Int("multi_panel", multiPanel.Id), zap.Int("new_multi_panel", multiPanelId))
				multiPanelIdMap[multiPanel.Id] = multiPanelId
			}
		}

		if len(failedMultiPanels) == 0 {
			successfulItems = append(successfulItems, "Multi Panels")
		} else {
			failedItems = append(failedItems, fmt.Sprintf("Multi Panels (x%d)", len(failedMultiPanels)))
		}

		// Import Multi Panel Targets
		log.Logger.Info("Importing multi panel targets", zap.Uint64("guild", guildId))
		failedMultiPanelTargets := 0
		for multiPanelId, panelIds := range data.MultiPanelTargets {
			if len(failedMultiPanels) > 0 && utils.Contains(failedMultiPanels, multiPanelId) {
				skippedItems = append(skippedItems, fmt.Sprintf("Multi Panel Targets (Multi Panel: %d)", multiPanelId))
				continue
			}
			for _, panelId := range panelIds {
				if len(failedPanels) > 0 && utils.Contains(failedPanels, panelId) {
					skippedItems = append(skippedItems, fmt.Sprintf("Multi Panel Targets (Multi Panel: %d, Panel: %d)", multiPanelId, panelId))
					continue
				}
				if err := dbclient.Client.MultiPanelTargets.Insert(queryCtx, multiPanelIdMap[multiPanelId], panelIdMap[panelId]); err != nil {
					failedMultiPanelTargets++
				}
			}
		}

		if failedMultiPanelTargets == 0 {
			successfulItems = append(successfulItems, "Multi Panel Targets")
		} else {
			failedItems = append(failedItems, fmt.Sprintf("Multi Panel Targets (x%d)", failedMultiPanelTargets))
		}

		if data.Settings.ContextMenuPanel != nil {
			newContextMenuPanel := panelIdMap[*data.Settings.ContextMenuPanel]
			data.Settings.ContextMenuPanel = &newContextMenuPanel
		}

		// Import settings
		log.Logger.Info("Importing settings", zap.Uint64("guild", guildId))
		if err := dbclient.Client.Settings.Set(queryCtx, guildId, data.Settings); err != nil {
			failedItems = append(failedItems, "Settings")
		} else {
			successfulItems = append(successfulItems, "Settings")
		}

		ticketCount, err := dbclient.Client.Tickets.GetTotalTicketCount(queryCtx, guildId)
		if err != nil {
			ctx.JSON(500, utils.ErrorJson(err))
			return
		}
		ticketsToCreate := make([]database2.Ticket, len(data.Tickets))
		ticketIdMapTwo := make(map[int]int)

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

				ticketIdMapTwo[ticket.Id] = ticket.Id + ticketCount
			} else {
				skippedItems = append(skippedItems, fmt.Sprintf("Ticket (ID: %d)", ticket.Id))
			}
		}

		log.Logger.Info("Importing tickets", zap.Uint64("guild", guildId))
		if err := dbclient.Client2.Tickets.BulkImport(queryCtx, guildId, ticketsToCreate); err != nil {
			failedItems = append(failedItems, "Tickets")
		} else {
			successfulItems = append(successfulItems, "Tickets")
		}

		// Update the mapping
		log.Logger.Info("Importing mapping for tickets", zap.Uint64("guild", guildId))
		for area, m := range map[string]map[int]int{"ticket": ticketIdMapTwo} {
			for sourceId, targetId := range m {
				if err := dbclient.Client2.ImportMappingTable.Set(queryCtx, guildId, area, sourceId, targetId); err != nil {
					ctx.JSON(500, utils.ErrorJson(err))
					return
				}
			}
		}

		ticketsExtrasGroup, _ := errgroup.WithContext(queryCtx)

		ticketsExtrasGroup.Go(func() (err error) {
			log.Logger.Info("Importing ticket members", zap.Uint64("guild", guildId))
			newMembersMap := make(map[int][]uint64)
			for ticketId, members := range data.TicketAdditionalMembers {
				if _, ok := ticketIdMap[ticketId]; !ok {
					continue
				}

				newMembersMap[ticketIdMap[ticketId]] = members
			}

			if err := dbclient.Client2.TicketMembers.ImportBulk(queryCtx, guildId, newMembersMap); err != nil {
				failedItems = append(failedItems, "Ticket Additional Members")
			} else {
				successfulItems = append(successfulItems, "Ticket Additional Members")
			}

			return
		})

		// Import ticket last messages
		ticketsExtrasGroup.Go(func() (err error) {
			log.Logger.Info("Importing ticket last messages", zap.Uint64("guild", guildId))
			msgs := map[int]database2.TicketLastMessage{}
			for _, msg := range data.TicketLastMessages {
				if _, ok := ticketIdMap[msg.TicketId]; !ok {
					continue
				}

				msgs[ticketIdMap[msg.TicketId]] = database2.TicketLastMessage{
					LastMessageId:   msg.Data.LastMessageId,
					LastMessageTime: msg.Data.LastMessageTime,
					UserId:          msg.Data.UserId,
					UserIsStaff:     msg.Data.UserIsStaff,
				}
			}

			if err := dbclient.Client2.TicketLastMessage.ImportBulk(queryCtx, guildId, msgs); err != nil {
				failedItems = append(failedItems, "Ticket Last Messages")
			} else {
				successfulItems = append(successfulItems, "Ticket Last Messages")
			}
			return
		})

		ticketsExtrasGroup.Go(func() (err error) {
			log.Logger.Info("Importing ticket claims", zap.Uint64("guild", guildId))
			newClaimsMap := make(map[int]uint64)
			for ticketId, user := range data.TicketClaims {
				if _, ok := ticketIdMap[ticketId]; !ok {
					continue
				}
				newClaimsMap[ticketIdMap[ticketId]] = user.Data
			}

			if err := dbclient.Client2.TicketClaims.ImportBulk(queryCtx, guildId, newClaimsMap); err != nil {
				failedItems = append(failedItems, "Ticket Claims")
			} else {
				successfulItems = append(successfulItems, "Ticket Claims")
			}
			return
		})

		// Import ticket ratings
		ticketsExtrasGroup.Go(func() (err error) {
			log.Logger.Info("Importing ticket ratings", zap.Uint64("guild", guildId))
			newRatingsMap := make(map[int]uint8)
			for ticketId, rating := range data.ServiceRatings {
				if _, ok := ticketIdMapTwo[ticketId]; !ok {
					continue
				}
				newRatingsMap[ticketIdMapTwo[ticketId]] = uint8(rating.Data)
			}

			if err := dbclient.Client2.ServiceRatings.ImportBulk(queryCtx, guildId, newRatingsMap); err != nil {
				failedItems = append(failedItems, "Ticket Ratings")
			} else {
				successfulItems = append(successfulItems, "Ticket Ratings")
			}
			return
		})

		// Import participants
		ticketsExtrasGroup.Go(func() (err error) {
			log.Logger.Info("Importing ticket participants", zap.Uint64("guild", guildId))
			newParticipantsMap := make(map[int][]uint64)
			for ticketId, participants := range data.Participants {
				if _, ok := ticketIdMapTwo[ticketId]; !ok {
					continue
				}
				newParticipantsMap[ticketIdMapTwo[ticketId]] = participants
			}

			if err := dbclient.Client2.Participants.ImportBulk(queryCtx, guildId, newParticipantsMap); err != nil {
				failedItems = append(failedItems, "Ticket Participants")
			} else {
				successfulItems = append(successfulItems, "Ticket Participants")
			}
			return
		})

		// Import First Response Times
		ticketsExtrasGroup.Go(func() (err error) {
			failedFirstResponseTimes := 0
			log.Logger.Info("Importing first response times", zap.Uint64("guild", guildId))
			for _, frt := range data.FirstResponseTimes {
				if _, ok := ticketIdMapTwo[frt.TicketId]; !ok {
					continue
				}
				if err := dbclient.Client.FirstResponseTime.Set(queryCtx, guildId, frt.UserId, ticketIdMapTwo[frt.TicketId], frt.ResponseTime); err != nil {
					failedFirstResponseTimes++
				}
			}

			if failedFirstResponseTimes == 0 {
				successfulItems = append(successfulItems, "First Response Times")
			} else {
				failedItems = append(failedItems, fmt.Sprintf("First Response Times (x%d)", failedFirstResponseTimes))
			}
			return
		})

		ticketsExtrasGroup.Go(func() (err error) {
			log.Logger.Info("Importing ticket survey responses", zap.Uint64("guild", guildId))
			failedSurveyResponses := 0
			for _, response := range data.ExitSurveyResponses {
				if _, ok := ticketIdMapTwo[response.TicketId]; !ok {
					continue
				}
				resps := map[int]string{
					*response.Data.QuestionId: *response.Data.Response,
				}

				if err := dbclient.Client.ExitSurveyResponses.AddResponses(queryCtx, guildId, ticketIdMapTwo[response.TicketId], formIdMap[*response.Data.FormId], resps); err != nil {
					failedSurveyResponses++
				}
			}

			if failedSurveyResponses == 0 {
				successfulItems = append(successfulItems, "Exit Survey Responses")
			} else {
				failedItems = append(failedItems, fmt.Sprintf("Exit Survey Responses (x%d)", failedSurveyResponses))
			}
			return
		})

		// Import Close Reasons
		ticketsExtrasGroup.Go(func() (err error) {
			log.Logger.Info("Importing close reasons", zap.Uint64("guild", guildId))
			failedCloseReasons := 0
			for _, reason := range data.CloseReasons {
				if _, ok := ticketIdMapTwo[reason.TicketId]; !ok {
					continue
				}
				if err := dbclient.Client.CloseReason.Set(queryCtx, guildId, ticketIdMapTwo[reason.TicketId], reason.Data); err != nil {
					failedCloseReasons++
				}
			}

			if failedCloseReasons == 0 {
				successfulItems = append(successfulItems, "Close Reasons")
			} else {
				failedItems = append(failedItems, fmt.Sprintf("Close Reasons (x%d)", failedCloseReasons))
			}
			return
		})

		// Import Autoclose Excluded Tickets
		ticketsExtrasGroup.Go(func() (err error) {
			failedAutocloseExcluded := 0
			log.Logger.Info("Importing autoclose excluded tickets", zap.Uint64("guild", guildId))
			for _, ticketId := range data.AutocloseExcluded {
				if _, ok := ticketIdMapTwo[ticketId]; !ok {
					continue
				}
				if err := dbclient.Client.AutoCloseExclude.Exclude(queryCtx, guildId, ticketIdMapTwo[ticketId]); err != nil {
					failedAutocloseExcluded++
				}
			}

			if failedAutocloseExcluded == 0 {
				successfulItems = append(successfulItems, "Autoclose Excluded Tickets")
			} else {
				failedItems = append(failedItems, fmt.Sprintf("Autoclose Excluded Tickets (x%d)", failedAutocloseExcluded))
			}
			return
		})

		// Import Archive Messages
		ticketsExtrasGroup.Go(func() (err error) {
			log.Logger.Info("Importing archive messages", zap.Uint64("guild", guildId))
			failedArchiveMessages := 0
			for _, message := range data.ArchiveMessages {
				if _, ok := ticketIdMapTwo[message.TicketId]; !ok {
					continue
				}
				if err := dbclient.Client.ArchiveMessages.Set(queryCtx, guildId, ticketIdMapTwo[message.TicketId], message.Data.ChannelId, message.Data.MessageId); err != nil {
					failedArchiveMessages++
				}
			}

			if failedArchiveMessages == 0 {
				successfulItems = append(successfulItems, "Archive Messages")
			} else {
				failedItems = append(failedItems, fmt.Sprintf("Archive Messages (x%d)", failedArchiveMessages))
			}
			return
		})

		_ = ticketsExtrasGroup.Wait()
	}

	ctx.JSON(200, map[string]interface{}{
		"failed":  failedItems,
		"success": successfulItems,
		"skipped": skippedItems,
	})
}
