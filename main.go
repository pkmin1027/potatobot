package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/joho/godotenv"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

var (
	dg               *discordgo.Session
	err              error
	mongoClient      *mongo.Client
	ticketCollection *mongo.Collection
	guildID          = "1274752368063414292" // 서버 ID는 여기에 입력하세요.

	// [설정] 카테고리별 지원 역할 ID
	// 각 티켓 종류(Value)와 담당할 역할의 ID를 짝지어 입력하세요.
	categorySupportRoles = map[string]string{
		"일반민원": "1397231132579467294", // 일반민원 담당 역할 ID
		"법률구조": "1397231132579467294", // 법률구조 담당 역할 ID
		"부패신고": "1397981755847217325", // 부패신고 담당 역할 ID
	}

	// [설정] 기본 지원 역할 ID
	// 맵에 없는 카테고리가 선택되거나, 다른 명령어에서 사용할 기본 역할 ID
	defaultSupportRoleID = "1397231132579467294"
)

const (
	colorBlue   = 0x0099ff
	colorGreen  = 0x28a745
	colorRed    = 0xdc3545
	colorYellow = 0xffc107
)

var ticketOptions = []discordgo.SelectMenuOption{
	{Label: "일반민원", Value: "일반민원", Description: "행정민원, 파산신고, 사업신청은 해당 창구로 문의 바랍니다.", Emoji: &discordgo.ComponentEmoji{Name: "📄"}},
	{Label: "법률구조", Value: "법률구조", Description: "법률상담은 해당 창구로 문의 바랍니다.", Emoji: &discordgo.ComponentEmoji{Name: "⚖️"}},
	{Label: "부패신고", Value: "부패신고", Description: "공익신고, 금융신고는 해당 창구로 문의 바랍니다.", Emoji: &discordgo.ComponentEmoji{Name: "🗑️"}},
}

type counter struct {
	ID  string `bson:"_id"`
	Seq uint64 `bson:"seq"`
}

func main() {
	err = godotenv.Load()
	if err != nil {
		log.Fatalf("Error loading .env file: %v", err)
	}

	mongoURI := os.Getenv("MONGO_URI")
	dbName := os.Getenv("MONGO_DATABASE")
	collectionName := os.Getenv("MONGO_COLLECTION")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	mongoClient, err = mongo.Connect(ctx, options.Client().ApplyURI(mongoURI))
	if err != nil {
		log.Fatalf("Failed to connect to MongoDB: %v", err)
	}
	defer mongoClient.Disconnect(ctx)

	err = mongoClient.Ping(ctx, nil)
	if err != nil {
		log.Fatalf("Failed to ping MongoDB: %v", err)
	}
	log.Println("Successfully connected to MongoDB!")

	ticketCollection = mongoClient.Database(dbName).Collection(collectionName)

	token := os.Getenv("BOT_TOKEN")
	dg, err = discordgo.New("Bot " + token)
	if err != nil {
		log.Fatalf("Error creating Discord session: %v", err)
	}

	dg.Identify.Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildMessages
	dg.AddHandler(ready)
	dg.AddHandler(interactionCreate)

	err = dg.Open()
	if err != nil {
		log.Fatalf("Error opening connection: %v", err)
	}
	defer dg.Close()

	registerCommands()

	fmt.Println("Bot is now running. Press CTRL+C to exit.")
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)
	<-sc
}

func getNextSequenceValue(sequenceName string) (uint64, error) {
	filter := bson.M{"_id": sequenceName}
	update := bson.M{"$inc": bson.M{"seq": 1}}
	opts := options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After)

	var result counter
	err := ticketCollection.FindOneAndUpdate(context.TODO(), filter, update, opts).Decode(&result)
	if err != nil {
		return 0, fmt.Errorf("could not update sequence for '%s': %w", sequenceName, err)
	}
	return result.Seq, nil
}

func createTicketChannel(s *discordgo.Session, i *discordgo.InteractionCreate, topicValue string) {
	nextSeq, err := getNextSequenceValue(topicValue)
	if err != nil {
		log.Printf("Could not get next sequence for ticket: %v", err)
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Embeds: []*discordgo.MessageEmbed{{Title: "오류", Description: "티켓 번호를 생성하는 데 실패했습니다. 관리자에게 문의하세요.", Color: colorRed}},
				Flags:  discordgo.MessageFlagsEphemeral,
			},
		})
		return
	}

	supportRoleID, ok := categorySupportRoles[topicValue]
	if !ok {
		log.Printf("Warning: No support role configured for category '%s'. Falling back to default.", topicValue)
		supportRoleID = defaultSupportRoleID
	}

	ticketNumber := fmt.Sprintf("%04d", nextSeq)
	channelName := fmt.Sprintf("%s-%s", topicValue, ticketNumber)

	ch, err := s.GuildChannelCreateComplex(i.GuildID, discordgo.GuildChannelCreateData{
		Name:  channelName,
		Type:  discordgo.ChannelTypeGuildText,
		Topic: fmt.Sprintf("User: %s | Ticket ID: %s-%s", i.Member.User.Username, topicValue, ticketNumber),
		PermissionOverwrites: []*discordgo.PermissionOverwrite{
			{ID: i.GuildID, Type: discordgo.PermissionOverwriteTypeRole, Deny: discordgo.PermissionViewChannel},
			{ID: i.Member.User.ID, Type: discordgo.PermissionOverwriteTypeMember, Allow: discordgo.PermissionViewChannel | discordgo.PermissionSendMessages},
			{ID: supportRoleID, Type: discordgo.PermissionOverwriteTypeRole, Allow: discordgo.PermissionViewChannel | discordgo.PermissionSendMessages},
		},
	})
	if err != nil {
		log.Printf("Error creating ticket channel: %v", err)
		return
	}

	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Embeds: []*discordgo.MessageEmbed{{Title: "티켓 채널 생성 완료", Description: fmt.Sprintf("성공적으로 <#%s> 채널을 생성했습니다.", ch.ID), Color: colorGreen}},
			Flags:  discordgo.MessageFlagsEphemeral,
		},
	})

	welcomeEmbed := &discordgo.MessageEmbed{
		Title:       fmt.Sprintf("%s (#%s)", topicValue, ticketNumber),
		Description: fmt.Sprintf("안녕하세요, <@%s>님! 문의주셔서 감사합니다.\n담당 직원(<@&%s>)이 곧 내용을 확인할 것입니다.", i.Member.User.ID, supportRoleID),
		Color:       colorBlue,
		Timestamp:   time.Now().Format(time.RFC3339),
	}
	s.ChannelMessageSendEmbed(ch.ID, welcomeEmbed)
}

func ready(s *discordgo.Session, event *discordgo.Ready) {
	log.Printf("Logged in as: %v#%v", s.State.User.Username, s.State.User.Discriminator)
}

func registerCommands() {
	commands := []*discordgo.ApplicationCommand{
		{Name: "panel", Description: "티켓 생성 패널을 현재 채널에 보냅니다."},
		{Name: "close", Description: "현재 티켓 채널을 닫습니다."},
		{Name: "add", Description: "티켓에 사용자를 추가합니다.", Options: []*discordgo.ApplicationCommandOption{{Type: discordgo.ApplicationCommandOptionUser, Name: "user", Description: "추가할 사용자", Required: true}}},
		{Name: "remove", Description: "티켓에서 사용자를 제거합니다.", Options: []*discordgo.ApplicationCommandOption{{Type: discordgo.ApplicationCommandOptionUser, Name: "user", Description: "제거할 사용자", Required: true}}},
		{Name: "roleadd", Description: "티켓에 역할을 추가합니다.", Options: []*discordgo.ApplicationCommandOption{{Type: discordgo.ApplicationCommandOptionRole, Name: "role", Description: "추가할 역할", Required: true}}},
		{Name: "roleremove", Description: "티켓에서 역할을 제거합니다.", Options: []*discordgo.ApplicationCommandOption{{Type: discordgo.ApplicationCommandOptionRole, Name: "role", Description: "제거할 역할", Required: true}}},
	}
	for _, v := range commands {
		_, err := dg.ApplicationCommandCreate(dg.State.User.ID, guildID, v)
		if err != nil {
			log.Printf("Cannot create '%v' command: %v", v.Name, err)
		}
	}
}

func interactionCreate(s *discordgo.Session, i *discordgo.InteractionCreate) {
	switch i.Type {
	case discordgo.InteractionApplicationCommand:
		handleSlashCommands(s, i)
	case discordgo.InteractionMessageComponent:
		handleMessageComponent(s, i)
	}
}

func handleSlashCommands(s *discordgo.Session, i *discordgo.InteractionCreate) {
	data := i.ApplicationCommandData()
	switch data.Name {
	case "panel":
		sendTicketPanel(s, i)
	case "close":
		closeTicket(s, i)
	case "add":
		addUserToTicket(s, i)
	case "remove":
		removeUserFromTicket(s, i)
	case "roleadd":
		addRoleToTicket(s, i)
	case "roleremove":
		removeRoleFromTicket(s, i)
	}
}

func sendTicketPanel(s *discordgo.Session, i *discordgo.InteractionCreate) {
	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Embeds: []*discordgo.MessageEmbed{{Title: "강원특별자치도청 민원창구", Description: "아래 메뉴에서 원하시는 민원 창구를 선택하여 티켓을 생성해주세요.", Color: colorBlue}},
			Components: []discordgo.MessageComponent{
				discordgo.ActionsRow{
					Components: []discordgo.MessageComponent{
						discordgo.SelectMenu{
							CustomID:    "ticket_topic_select",
							Placeholder: "문의할 창구를 선택해주세요.",
							Options:     ticketOptions,
						},
					},
				},
			},
		},
	})
}

func handleMessageComponent(s *discordgo.Session, i *discordgo.InteractionCreate) {
	data := i.MessageComponentData()
	if data.CustomID == "ticket_topic_select" {
		selectedValue := data.Values[0]
		createTicketChannel(s, i, selectedValue)
	}
}

func closeTicket(s *discordgo.Session, i *discordgo.InteractionCreate) {
	ch, _ := s.Channel(i.ChannelID)
	if ch.Topic != "" {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Embeds: []*discordgo.MessageEmbed{{Title: "티켓 닫힘", Description: "요청에 따라 티켓을 닫습니다. 이 채널은 잠시 후 삭제됩니다.", Color: colorRed}},
			},
		})
		time.Sleep(5 * time.Second)
		_, err := s.ChannelDelete(i.ChannelID)
		if err != nil {
			log.Printf("Error closing ticket: %v", err)
		}
	} else {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Embeds: []*discordgo.MessageEmbed{{Title: "오류", Description: "이 명령어는 티켓 채널에서만 사용할 수 있습니다.", Color: colorRed}},
				Flags:  discordgo.MessageFlagsEphemeral,
			},
		})
	}
}

func addUserToTicket(s *discordgo.Session, i *discordgo.InteractionCreate) {
	user := i.ApplicationCommandData().Options[0].UserValue(s)
	ch, err := s.Channel(i.ChannelID)
	if err != nil {
		log.Printf("Could not get channel info: %v", err)
		return
	}
	for _, po := range ch.PermissionOverwrites {
		if po.Type == discordgo.PermissionOverwriteTypeMember && po.ID == user.ID {
			if (po.Allow & discordgo.PermissionViewChannel) == discordgo.PermissionViewChannel {
				s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
					Type: discordgo.InteractionResponseChannelMessageWithSource,
					Data: &discordgo.InteractionResponseData{
						Embeds: []*discordgo.MessageEmbed{{Title: "이미 추가된 사용자", Description: fmt.Sprintf("<@%s> 님은 이미 이 티켓에 참여하고 있습니다.", user.ID), Color: colorYellow}},
						Flags:  discordgo.MessageFlagsEphemeral,
					},
				})
				return
			}
		}
	}
	err = s.ChannelPermissionSet(i.ChannelID, user.ID, discordgo.PermissionOverwriteTypeMember, discordgo.PermissionViewChannel|discordgo.PermissionSendMessages, 0)
	if err != nil {
		log.Printf("Error adding user to ticket: %v", err)
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Embeds: []*discordgo.MessageEmbed{{Title: "오류", Description: "티켓에 사용자를 추가하는 데 실패했습니다.", Color: colorRed}},
				Flags:  discordgo.MessageFlagsEphemeral,
			},
		})
		return
	}
	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Embeds: []*discordgo.MessageEmbed{{Title: "사용자 추가", Description: fmt.Sprintf("<@%s> 님을 티켓에 추가했습니다.", user.ID), Color: colorGreen}}},
	})
}

func addRoleToTicket(s *discordgo.Session, i *discordgo.InteractionCreate) {
	role := i.ApplicationCommandData().Options[0].RoleValue(s, i.GuildID)
	ch, err := s.Channel(i.ChannelID)
	if err != nil {
		log.Printf("Could not get channel info: %v", err)
		return
	}
	if ch.Topic == "" {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Embeds: []*discordgo.MessageEmbed{{Title: "오류", Description: "이 명령어는 티켓 채널에서만 사용할 수 있습니다.", Color: colorRed}},
				Flags:  discordgo.MessageFlagsEphemeral,
			},
		})
		return
	}
	for _, po := range ch.PermissionOverwrites {
		if po.Type == discordgo.PermissionOverwriteTypeRole && po.ID == role.ID {
			if (po.Allow & discordgo.PermissionViewChannel) == discordgo.PermissionViewChannel {
				s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
					Type: discordgo.InteractionResponseChannelMessageWithSource,
					Data: &discordgo.InteractionResponseData{
						Embeds: []*discordgo.MessageEmbed{{Title: "이미 추가된 역할", Description: fmt.Sprintf("<@&%s> 역할은 이미 이 티켓에 참여하고 있습니다.", role.ID), Color: colorYellow}},
						Flags:  discordgo.MessageFlagsEphemeral,
					},
				})
				return
			}
		}
	}
	err = s.ChannelPermissionSet(i.ChannelID, role.ID, discordgo.PermissionOverwriteTypeRole, discordgo.PermissionViewChannel|discordgo.PermissionSendMessages, 0)
	if err != nil {
		log.Printf("Error adding role to ticket: %v", err)
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Embeds: []*discordgo.MessageEmbed{{Title: "오류", Description: "티켓에 역할을 추가하는 데 실패했습니다.", Color: colorRed}},
				Flags:  discordgo.MessageFlagsEphemeral,
			},
		})
		return
	}
	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Embeds: []*discordgo.MessageEmbed{{Title: "역할 추가", Description: fmt.Sprintf("<@&%s> 역할을 티켓에 추가했습니다.", role.ID), Color: colorGreen}},
		},
	})
}

func isConfiguredSupportRole(roleID string) bool {
	if roleID == defaultSupportRoleID {
		return true
	}
	for _, id := range categorySupportRoles {
		if id == roleID {
			return true
		}
	}
	return false
}

func removeUserFromTicket(s *discordgo.Session, i *discordgo.InteractionCreate) {
	user := i.ApplicationCommandData().Options[0].UserValue(s)
	err := s.ChannelPermissionDelete(i.ChannelID, user.ID)
	if err != nil {
		log.Printf("Error removing user from ticket: %v", err)
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Embeds: []*discordgo.MessageEmbed{{Title: "오류", Description: "티켓에서 사용자를 제거하는 데 실패했습니다.", Color: colorRed}},
				Flags:  discordgo.MessageFlagsEphemeral,
			},
		})
		return
	}
	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Embeds: []*discordgo.MessageEmbed{{Title: "사용자 제거", Description: fmt.Sprintf("<@%s> 님을 티켓에서 제거했습니다.", user.ID), Color: colorYellow}},
		},
	})
}

func removeRoleFromTicket(s *discordgo.Session, i *discordgo.InteractionCreate) {
	role := i.ApplicationCommandData().Options[0].RoleValue(s, i.GuildID)
	ch, err := s.Channel(i.ChannelID)
	if err != nil {
		log.Printf("Could not get channel info: %v", err)
		return
	}
	if ch.Topic == "" {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Embeds: []*discordgo.MessageEmbed{{Title: "오류", Description: "이 명령어는 티켓 채널에서만 사용할 수 있습니다.", Color: colorRed}},
				Flags:  discordgo.MessageFlagsEphemeral,
			},
		})
		return
	}

	if isConfiguredSupportRole(role.ID) {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Embeds: []*discordgo.MessageEmbed{{Title: "제거 불가", Description: "기본 지원 역할은 티켓에서 제거할 수 없습니다.", Color: colorRed}},
				Flags:  discordgo.MessageFlagsEphemeral,
			},
		})
		return
	}

	hasPermissions := false
	for _, po := range ch.PermissionOverwrites {
		if po.Type == discordgo.PermissionOverwriteTypeRole && po.ID == role.ID {
			hasPermissions = true
			break
		}
	}
	if !hasPermissions {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Embeds: []*discordgo.MessageEmbed{{Title: "역할 없음", Description: fmt.Sprintf("<@&%s> 역할은 이 티켓에 추가되어 있지 않습니다.", role.ID), Color: colorYellow}},
				Flags:  discordgo.MessageFlagsEphemeral,
			},
		})
		return
	}

	err = s.ChannelPermissionDelete(i.ChannelID, role.ID)
	if err != nil {
		log.Printf("Error removing role from ticket: %v", err)
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Embeds: []*discordgo.MessageEmbed{{Title: "오류", Description: "티켓에서 역할을 제거하는 데 실패했습니다.", Color: colorRed}},
				Flags:  discordgo.MessageFlagsEphemeral,
			},
		})
		return
	}

	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Embeds: []*discordgo.MessageEmbed{{Title: "역할 제거", Description: fmt.Sprintf("<@&%s> 역할을 티켓에서 제거했습니다.", role.ID), Color: colorYellow}},
		},
	})
}
