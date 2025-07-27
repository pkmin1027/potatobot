package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	// [수정됨] "github.comcom" -> "github.com" 오타 수정
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
	guildID          = "1274752368063414292" // 길드 ID 적용

	// 카테고리별 지원 역할 ID 적용
	categorySupportRoles = map[string]string{
		"일반민원": "1397231132579467294",
		"법률구조": "1397231132579467294",
		"부패신고": "1397981755847217325",
	}

	// 기본 지원 역할 ID 적용
	defaultSupportRoleID = "1397231132579467294"
)

// 임베드 및 카테고리 ID를 미리 정의합니다.
const (
	colorBlue   = 0x0099ff
	colorGreen  = 0x28a745
	colorRed    = 0xdc3545
	colorYellow = 0xffc107
	colorGray   = 0x95a5a6

	// 티켓 카테고리 ID
	openTicketsCategoryID   = "1398719413016072306"
	closedTicketsCategoryID = "1398719595384406137"
)

// 패널의 드롭다운 메뉴에 표시될 옵션입니다.
var ticketOptions = []discordgo.SelectMenuOption{
	{Label: "일반민원", Value: "일반민원", Description: "행정민원, 파산신고, 사업신청은 해당 창구로 문의 바랍니다.", Emoji: &discordgo.ComponentEmoji{Name: "📄"}},
	{Label: "법률구조", Value: "법률구조", Description: "법률상담은 해당 창구로 문의 바랍니다.", Emoji: &discordgo.ComponentEmoji{Name: "⚖️"}},
	{Label: "부패신고", Value: "부패신고", Description: "공익신고, 금융신고는 해당 창구로 문의 바랍니다.", Emoji: &discordgo.ComponentEmoji{Name: "🗑️"}},
}

// MongoDB 카운터 문서의 구조체입니다.
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
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseChannelMessageWithSource, Data: &discordgo.InteractionResponseData{Embeds: []*discordgo.MessageEmbed{{Title: "오류", Description: "티켓 번호를 생성하는 데 실패했습니다. 관리자에게 문의하세요.", Color: colorRed}}, Flags: discordgo.MessageFlagsEphemeral}})
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
		Name:     channelName,
		Type:     discordgo.ChannelTypeGuildText,
		Topic:    fmt.Sprintf("User ID: %s | Ticket ID: %s-%s", i.Member.User.ID, topicValue, ticketNumber),
		ParentID: openTicketsCategoryID, // 카테고리 지정
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
	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseChannelMessageWithSource, Data: &discordgo.InteractionResponseData{Embeds: []*discordgo.MessageEmbed{{Title: "티켓 채널 생성 완료", Description: fmt.Sprintf("성공적으로 <#%s> 채널을 생성했습니다.", ch.ID), Color: colorGreen}}, Flags: discordgo.MessageFlagsEphemeral}})
	messageData := &discordgo.MessageSend{
		Content: fmt.Sprintf("<@&%s>", supportRoleID),
		Embeds: []*discordgo.MessageEmbed{{
			Title:       fmt.Sprintf("%s (#%s)", topicValue, ticketNumber),
			Description: fmt.Sprintf("안녕하세요, <@%s>님! 문의주셔서 감사합니다.\n곧 담당자가 도착할 예정입니다. 잠시만 기다려주십시오.", i.Member.User.ID),
			Color:       colorBlue,
			Timestamp:   time.Now().Format(time.RFC3339),
		}},
		Components: []discordgo.MessageComponent{
			discordgo.ActionsRow{
				Components: []discordgo.MessageComponent{
					discordgo.Button{Label: "티켓 닫기", Style: discordgo.DangerButton, CustomID: "close_ticket_request"},
					discordgo.Button{Label: "담당자 배정", Style: discordgo.SuccessButton, CustomID: "claim_ticket"},
				},
			},
		},
	}
	s.ChannelMessageSendComplex(ch.ID, messageData)
}

func ready(s *discordgo.Session, event *discordgo.Ready) {
	log.Printf("Logged in as: %v#%v", s.State.User.Username, s.State.User.Discriminator)
}

func registerCommands() {
	commands := []*discordgo.ApplicationCommand{
		{Name: "패널", Description: "티켓 생성 패널을 현재 채널에 보냅니다."},
		{Name: "닫기", Description: "현재 티켓 채널을 닫습니다."},
		{Name: "추가", Description: "티켓에 사용자를 추가합니다.", Options: []*discordgo.ApplicationCommandOption{{Type: discordgo.ApplicationCommandOptionUser, Name: "user", Description: "추가할 사용자", Required: true}}},
		{Name: "제거", Description: "티켓에서 사용자를 제거합니다.", Options: []*discordgo.ApplicationCommandOption{{Type: discordgo.ApplicationCommandOptionUser, Name: "user", Description: "제거할 사용자", Required: true}}},
		{Name: "역할추가", Description: "티켓에 역할을 추가합니다.", Options: []*discordgo.ApplicationCommandOption{{Type: discordgo.ApplicationCommandOptionRole, Name: "role", Description: "추가할 역할", Required: true}}},
		{Name: "역할제거", Description: "티켓에서 역할을 제거합니다.", Options: []*discordgo.ApplicationCommandOption{{Type: discordgo.ApplicationCommandOptionRole, Name: "role", Description: "제거할 역할", Required: true}}},
		{Name: "담당자변경", Description: "티켓의 담당자를 변경합니다.", Options: []*discordgo.ApplicationCommandOption{{Type: discordgo.ApplicationCommandOptionUser, Name: "user", Description: "새로 지정할 담당자", Required: true}}},
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
	case "패널":
		sendTicketPanel(s, i)
	case "닫기":
		closeTicket(s, i)
	case "추가":
		addUserToTicket(s, i)
	case "제거":
		removeUserFromTicket(s, i)
	case "역할추가":
		addRoleToTicket(s, i)
	case "역할제거":
		removeRoleFromTicket(s, i)
	case "담당자변경":
		handleChangeAssignee(s, i)
	}
}

func handleMessageComponent(s *discordgo.Session, i *discordgo.InteractionCreate) {
	data := i.MessageComponentData()
	switch data.CustomID {
	case "ticket_topic_select":
		createTicketChannel(s, i, data.Values[0])
	case "close_ticket_request":
		handleCloseRequest(s, i)
	case "confirm_close_ticket":
		handleConfirmClose(s, i)
	case "cancel_close_ticket":
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseUpdateMessage})
		s.ChannelMessageDelete(i.ChannelID, i.Message.ID)
	case "claim_ticket":
		handleClaimTicket(s, i)
	case "reopen_ticket":
		handleReopenTicket(s, i)
	case "delete_ticket_permanent":
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseChannelMessageWithSource, Data: &discordgo.InteractionResponseData{Embeds: []*discordgo.MessageEmbed{{Title: "채널 삭제", Description: "5초 후 이 채널을 영구적으로 삭제합니다.", Color: colorRed}}}})
		time.Sleep(5 * time.Second)
		s.ChannelDelete(i.ChannelID)
	}
}

func sendTicketPanel(s *discordgo.Session, i *discordgo.InteractionCreate) {
	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseChannelMessageWithSource, Data: &discordgo.InteractionResponseData{Embeds: []*discordgo.MessageEmbed{{Title: "강원특별자치도청 민원창구", Description: "아래 메뉴에서 원하시는 민원 창구를 선택하여 티켓을 생성해주세요.", Color: colorBlue}}, Components: []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{discordgo.SelectMenu{CustomID: "ticket_topic_select", Placeholder: "문의할 창구를 선택해주세요.", Options: ticketOptions}}}}}})
}

func handleCloseRequest(s *discordgo.Session, i *discordgo.InteractionCreate) {
	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseChannelMessageWithSource, Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral, Embeds: []*discordgo.MessageEmbed{{Title: "닫기 확인", Description: "정말로 티켓을 닫으시겠습니까?\n닫힌 티켓은 관리자만 다시 열 수 있습니다.", Color: colorYellow}}, Components: []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{discordgo.Button{Label: "닫기 확인", Style: discordgo.DangerButton, CustomID: "confirm_close_ticket"}, discordgo.Button{Label: "취소", Style: discordgo.SecondaryButton, CustomID: "cancel_close_ticket"}}}}}})
}

func handleConfirmClose(s *discordgo.Session, i *discordgo.InteractionCreate) {
	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseUpdateMessage, Data: &discordgo.InteractionResponseData{Embeds: []*discordgo.MessageEmbed{{Title: "처리 중...", Description: "티켓을 닫고 보관 처리하고 있습니다.", Color: colorGray}}, Components: []discordgo.MessageComponent{}}})
	ch, _ := s.Channel(i.ChannelID)
	userID := getUserIDFromTopic(ch.Topic)
	if userID == "" {
		log.Println("Error: Could not find user ID in channel topic.")
		return
	}
	s.ChannelPermissionSet(ch.ID, userID, discordgo.PermissionOverwriteTypeMember, 0, discordgo.PermissionViewChannel)
	_, err := s.ChannelEditComplex(ch.ID, &discordgo.ChannelEdit{
		ParentID: closedTicketsCategoryID,
	})
	if err != nil {
		log.Printf("Error moving channel to closed category: %v", err)
	}
	adminPanel := &discordgo.MessageSend{Embeds: []*discordgo.MessageEmbed{{Title: "관리자 패널", Description: fmt.Sprintf("<@%s> 님이 티켓을 닫았습니다. 아래 버튼을 사용하여 티켓을 관리하세요.", i.Member.User.ID), Color: colorGray}}, Components: []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{discordgo.Button{Label: "티켓 재오픈", Style: discordgo.SuccessButton, CustomID: "reopen_ticket"}, discordgo.Button{Label: "티켓 삭제", Style: discordgo.DangerButton, CustomID: "delete_ticket_permanent"}}}}}
	s.ChannelMessageSendComplex(ch.ID, adminPanel)
	s.ChannelMessageDelete(i.ChannelID, i.Message.ID)
}

func handleClaimTicket(s *discordgo.Session, i *discordgo.InteractionCreate) {
	originalEmbed := i.Message.Embeds[0]
	for _, field := range originalEmbed.Fields {
		if field.Name == "담당자" {
			s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseChannelMessageWithSource, Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral, Embeds: []*discordgo.MessageEmbed{{Title: "오류", Description: "이미 담당자가 배정된 티켓입니다.", Color: colorRed}}}})
			return
		}
	}
	originalEmbed.Fields = append(originalEmbed.Fields, &discordgo.MessageEmbedField{Name: "담당자", Value: i.Member.Mention(), Inline: false})
	components := i.Message.Components
	for _, row := range components {
		if actionsRow, ok := row.(*discordgo.ActionsRow); ok {
			for j, comp := range actionsRow.Components {
				if button, ok := comp.(*discordgo.Button); ok {
					if button.CustomID == "claim_ticket" {
						button.Disabled = true
						actionsRow.Components[j] = button
					}
				}
			}
		}
	}
	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseUpdateMessage, Data: &discordgo.InteractionResponseData{Embeds: []*discordgo.MessageEmbed{originalEmbed}, Components: components}})
	s.ChannelMessageSendEmbed(i.ChannelID, &discordgo.MessageEmbed{Title: "담당자 배정", Description: fmt.Sprintf("<@%s> 님이 이 티켓의 담당자로 배정되었습니다.", i.Member.User.ID), Color: colorGreen})
}

func handleChangeAssignee(s *discordgo.Session, i *discordgo.InteractionCreate) {
	targetUser := i.ApplicationCommandData().Options[0].UserValue(s)
	executor := i.Member
	ch, _ := s.Channel(i.ChannelID)
	if !strings.Contains(ch.Topic, "User ID:") {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseChannelMessageWithSource, Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral, Embeds: []*discordgo.MessageEmbed{{Title: "오류", Description: "이 명령어는 티켓 채널에서만 사용할 수 있습니다.", Color: colorRed}}}})
		return
	}
	var ticketMessage *discordgo.Message
	messages, err := s.ChannelMessages(i.ChannelID, 100, "", "", "")
	if err != nil {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseChannelMessageWithSource, Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral, Embeds: []*discordgo.MessageEmbed{{Title: "오류", Description: "티켓 정보를 찾는 데 실패했습니다.", Color: colorRed}}}})
		return
	}
	for _, msg := range messages {
		if msg.Author.ID == s.State.User.ID && len(msg.Embeds) > 0 && len(msg.Components) > 0 {
			ticketMessage = msg
			break
		}
	}
	if ticketMessage == nil {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseChannelMessageWithSource, Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral, Embeds: []*discordgo.MessageEmbed{{Title: "오류", Description: "원본 티켓 메시지를 찾을 수 없습니다.", Color: colorRed}}}})
		return
	}
	isManager := false
	for _, roleID := range executor.Roles {
		if isConfiguredSupportRole(roleID) {
			isManager = true
			break
		}
	}
	currentAssigneeID := ""
	for _, field := range ticketMessage.Embeds[0].Fields {
		if field.Name == "담당자" {
			currentAssigneeID = strings.Trim(field.Value, "<@!>")
		}
	}
	if !isManager && executor.User.ID != currentAssigneeID {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseChannelMessageWithSource, Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral, Embeds: []*discordgo.MessageEmbed{{Title: "권한 없음", Description: "관리자 또는 현재 담당자만 이 명령어를 사용할 수 있습니다.", Color: colorRed}}}})
		return
	}
	canSeeChannel := false
	for _, po := range ch.PermissionOverwrites {
		if po.ID == targetUser.ID && (po.Allow&discordgo.PermissionViewChannel) == discordgo.PermissionViewChannel {
			canSeeChannel = true
			break
		}
	}
	if !canSeeChannel {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseChannelMessageWithSource, Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral, Embeds: []*discordgo.MessageEmbed{{Title: "오류", Description: fmt.Sprintf("%s 님은 이 채널을 볼 수 없어 담당자로 지정할 수 없습니다.", targetUser.Username), Color: colorRed}}}})
		return
	}
	originalEmbed := ticketMessage.Embeds[0]
	assigneeFieldExists := false
	for _, field := range originalEmbed.Fields {
		if field.Name == "담당자" {
			field.Value = targetUser.Mention()
			assigneeFieldExists = true
			break
		}
	}
	if !assigneeFieldExists {
		originalEmbed.Fields = append(originalEmbed.Fields, &discordgo.MessageEmbedField{Name: "담당자", Value: targetUser.Mention(), Inline: false})
	}
	for _, row := range ticketMessage.Components {
		if actionsRow, ok := row.(*discordgo.ActionsRow); ok {
			for j, comp := range actionsRow.Components {
				if button, ok := comp.(*discordgo.Button); ok && button.CustomID == "claim_ticket" && !button.Disabled {
					button.Disabled = true
					actionsRow.Components[j] = button
				}
			}
		}
	}
	editedEmbeds := []*discordgo.MessageEmbed{originalEmbed}
	_, err = s.ChannelMessageEditComplex(&discordgo.MessageEdit{
		Channel:    i.ChannelID,
		ID:         ticketMessage.ID,
		Embeds:     &editedEmbeds,
		Components: &ticketMessage.Components,
	})
	if err != nil {
		log.Printf("Error editing ticket message: %v", err)
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseChannelMessageWithSource, Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral, Embeds: []*discordgo.MessageEmbed{{Title: "오류", Description: "티켓 메시지를 수정하는 데 실패했습니다.", Color: colorRed}}}})
		return
	}
	s.ChannelMessageSendEmbed(i.ChannelID, &discordgo.MessageEmbed{
		Title:       "담당자 변경",
		Description: fmt.Sprintf("담당자가 <@%s> 님에서 <@%s> 님으로 변경되었습니다.", executor.User.ID, targetUser.ID),
		Color:       colorYellow,
	})
	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseChannelMessageWithSource, Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral, Embeds: []*discordgo.MessageEmbed{{Title: "성공", Description: "담당자를 성공적으로 변경했습니다.", Color: colorGreen}}}})
}

func handleReopenTicket(s *discordgo.Session, i *discordgo.InteractionCreate) {
	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseUpdateMessage})
	ch, _ := s.Channel(i.ChannelID)
	_, err := s.ChannelEditComplex(ch.ID, &discordgo.ChannelEdit{
		ParentID: openTicketsCategoryID,
	})
	if err != nil {
		log.Printf("Error moving channel to open category: %v", err)
	}
	userID := getUserIDFromTopic(ch.Topic)
	if userID == "" {
		log.Println("Error: Could not find user ID in channel topic.")
		return
	}
	s.ChannelPermissionSet(ch.ID, userID, discordgo.PermissionOverwriteTypeMember, discordgo.PermissionViewChannel, 0)
	s.ChannelMessageDelete(ch.ID, i.Message.ID)
	s.ChannelMessageSendEmbed(ch.ID, &discordgo.MessageEmbed{Title: "티켓 재오픈", Description: fmt.Sprintf("<@%s> 님이 티켓을 다시 열었습니다. <@%s>님, 다시 문의를 진행해주세요.", i.Member.User.ID, userID), Color: colorGreen})
}

func getUserIDFromTopic(topic string) string {
	parts := strings.Split(topic, "|")
	for _, part := range parts {
		if strings.Contains(part, "User ID:") {
			idPart := strings.TrimSpace(strings.TrimPrefix(part, "User ID:"))
			return idPart
		}
	}
	return ""
}

func closeTicket(s *discordgo.Session, i *discordgo.InteractionCreate) {
	ch, _ := s.Channel(i.ChannelID)
	if ch.Topic == "" {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseChannelMessageWithSource, Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral, Embeds: []*discordgo.MessageEmbed{{Title: "오류", Description: "이 명령어는 티켓 채널에서만 사용할 수 있습니다.", Color: colorRed}}}})
		return
	}
	handleCloseRequest(s, i)
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
				s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseChannelMessageWithSource, Data: &discordgo.InteractionResponseData{Embeds: []*discordgo.MessageEmbed{{Title: "이미 추가된 사용자", Description: fmt.Sprintf("<@%s> 님은 이미 이 티켓에 참여하고 있습니다.", user.ID), Color: colorYellow}}, Flags: discordgo.MessageFlagsEphemeral}})
				return
			}
		}
	}
	err = s.ChannelPermissionSet(i.ChannelID, user.ID, discordgo.PermissionOverwriteTypeMember, discordgo.PermissionViewChannel|discordgo.PermissionSendMessages, 0)
	if err != nil {
		log.Printf("Error adding user to ticket: %v", err)
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseChannelMessageWithSource, Data: &discordgo.InteractionResponseData{Embeds: []*discordgo.MessageEmbed{{Title: "오류", Description: "티켓에 사용자를 추가하는 데 실패했습니다.", Color: colorRed}}, Flags: discordgo.MessageFlagsEphemeral}})
		return
	}
	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseChannelMessageWithSource, Data: &discordgo.InteractionResponseData{Embeds: []*discordgo.MessageEmbed{{Title: "사용자 추가", Description: fmt.Sprintf("<@%s> 님을 티켓에 추가했습니다.", user.ID), Color: colorGreen}}}})
}

func addRoleToTicket(s *discordgo.Session, i *discordgo.InteractionCreate) {
	role := i.ApplicationCommandData().Options[0].RoleValue(s, i.GuildID)
	ch, err := s.Channel(i.ChannelID)
	if err != nil {
		log.Printf("Could not get channel info: %v", err)
		return
	}
	if ch.Topic == "" {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseChannelMessageWithSource, Data: &discordgo.InteractionResponseData{Embeds: []*discordgo.MessageEmbed{{Title: "오류", Description: "이 명령어는 티켓 채널에서만 사용할 수 있습니다.", Color: colorRed}}, Flags: discordgo.MessageFlagsEphemeral}})
		return
	}
	for _, po := range ch.PermissionOverwrites {
		if po.Type == discordgo.PermissionOverwriteTypeRole && po.ID == role.ID {
			if (po.Allow & discordgo.PermissionViewChannel) == discordgo.PermissionViewChannel {
				s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseChannelMessageWithSource, Data: &discordgo.InteractionResponseData{Embeds: []*discordgo.MessageEmbed{{Title: "이미 추가된 역할", Description: fmt.Sprintf("<@&%s> 역할은 이미 이 티켓에 참여하고 있습니다.", role.ID), Color: colorYellow}}, Flags: discordgo.MessageFlagsEphemeral}})
				return
			}
		}
	}
	err = s.ChannelPermissionSet(i.ChannelID, role.ID, discordgo.PermissionOverwriteTypeRole, discordgo.PermissionViewChannel|discordgo.PermissionSendMessages, 0)
	if err != nil {
		log.Printf("Error adding role to ticket: %v", err)
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseChannelMessageWithSource, Data: &discordgo.InteractionResponseData{Embeds: []*discordgo.MessageEmbed{{Title: "오류", Description: "티켓에 역할을 추가하는 데 실패했습니다.", Color: colorRed}}, Flags: discordgo.MessageFlagsEphemeral}})
		return
	}
	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseChannelMessageWithSource, Data: &discordgo.InteractionResponseData{Embeds: []*discordgo.MessageEmbed{{Title: "역할 추가", Description: fmt.Sprintf("<@&%s> 역할을 티켓에 추가했습니다.", role.ID), Color: colorGreen}}}})
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
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseChannelMessageWithSource, Data: &discordgo.InteractionResponseData{Embeds: []*discordgo.MessageEmbed{{Title: "오류", Description: "티켓에서 사용자를 제거하는 데 실패했습니다.", Color: colorRed}}, Flags: discordgo.MessageFlagsEphemeral}})
		return
	}
	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseChannelMessageWithSource, Data: &discordgo.InteractionResponseData{Embeds: []*discordgo.MessageEmbed{{Title: "사용자 제거", Description: fmt.Sprintf("<@%s> 님을 티켓에서 제거했습니다.", user.ID), Color: colorYellow}}}})
}

func removeRoleFromTicket(s *discordgo.Session, i *discordgo.InteractionCreate) {
	role := i.ApplicationCommandData().Options[0].RoleValue(s, i.GuildID)
	ch, err := s.Channel(i.ChannelID)
	if err != nil {
		log.Printf("Could not get channel info: %v", err)
		return
	}
	if ch.Topic == "" {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseChannelMessageWithSource, Data: &discordgo.InteractionResponseData{Embeds: []*discordgo.MessageEmbed{{Title: "오류", Description: "이 명령어는 티켓 채널에서만 사용할 수 있습니다.", Color: colorRed}}, Flags: discordgo.MessageFlagsEphemeral}})
		return
	}
	if isConfiguredSupportRole(role.ID) {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseChannelMessageWithSource, Data: &discordgo.InteractionResponseData{Embeds: []*discordgo.MessageEmbed{{Title: "제거 불가", Description: "기본 지원 역할은 티켓에서 제거할 수 없습니다.", Color: colorRed}}, Flags: discordgo.MessageFlagsEphemeral}})
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
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseChannelMessageWithSource, Data: &discordgo.InteractionResponseData{Embeds: []*discordgo.MessageEmbed{{Title: "역할 없음", Description: fmt.Sprintf("<@&%s> 역할은 이 티켓에 추가되어 있지 않습니다.", role.ID), Color: colorYellow}}, Flags: discordgo.MessageFlagsEphemeral}})
		return
	}
	err = s.ChannelPermissionDelete(i.ChannelID, role.ID)
	if err != nil {
		log.Printf("Error removing role from ticket: %v", err)
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseChannelMessageWithSource, Data: &discordgo.InteractionResponseData{Embeds: []*discordgo.MessageEmbed{{Title: "오류", Description: "티켓에서 역할을 제거하는 데 실패했습니다.", Color: colorRed}}, Flags: discordgo.MessageFlagsEphemeral}})
		return
	}
	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseChannelMessageWithSource, Data: &discordgo.InteractionResponseData{Embeds: []*discordgo.MessageEmbed{{Title: "역할 제거", Description: fmt.Sprintf("<@&%s> 역할을 티켓에서 제거했습니다.", role.ID), Color: colorYellow}}}})
}
