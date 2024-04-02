package imitate

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	http "github.com/bogdanfinn/fhttp"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/maxduke/go-chatgpt-api/api"
	"github.com/maxduke/go-chatgpt-api/api/chatgpt"
	"github.com/linweiyuan/go-logger/logger"
)

var (
	reg *regexp.Regexp
	token string
)

func init() {
	reg, _ = regexp.Compile("[^a-zA-Z0-9]+")
}

func CreateChatCompletions(c *gin.Context) {
	var original_request APIRequest
	err := c.BindJSON(&original_request)
	if err != nil {
		c.JSON(400, gin.H{"error": gin.H{
			"message": "Request must be proper JSON",
			"type":    "invalid_request_error",
			"param":   nil,
			"code":    err.Error(),
		}})
		return
	}

	authHeader := c.GetHeader(api.AuthorizationHeader)
	imitate_api_key := os.Getenv("IMITATE_API_KEY")
	if authHeader != "" {
		customAccessToken := strings.Replace(authHeader, "Bearer ", "", 1)
		// Check if customAccessToken starts with eyJhbGciOiJSUzI1NiI
		if strings.HasPrefix(customAccessToken, "eyJhbGciOiJSUzI1NiI") {
			token = customAccessToken
		// use defiend access token if the provided api key is equal to "IMITATE_API_KEY"
		} else if imitate_api_key != "" && customAccessToken == imitate_api_key {
			token = os.Getenv("IMITATE_ACCESS_TOKEN")
			if token == "" {
				token = api.IMITATE_accessToken
			}
		}
	}

	if token == "" {
		c.JSON(400, gin.H{"error": gin.H{
			"message": "API KEY is missing or invalid",
			"type":    "invalid_request_error",
			"param":   nil,
			"code":    "400",
		}})
		return
	}

	uid := uuid.NewString()
	var chat_require *chatgpt.ChatRequire
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		err = chatgpt.InitWSConn(token, uid)
	}()
	go func() {
		defer wg.Done()
		chat_require = chatgpt.CheckRequire(token)
	}()
	wg.Wait()
	if err != nil {
		c.JSON(500, gin.H{"error": "unable to create ws tunnel"})
		return
	}
	if chat_require == nil {
		c.JSON(500, gin.H{"error": "unable to check chat requirement"})
		return
	}

	// Convert the chat request to a ChatGPT request
	translated_request := convertAPIRequest(original_request, chat_require.Arkose.Required)

	response, done := sendConversationRequest(c, translated_request, token, chat_require.Token)
	if done {
		return
	}

	defer func(Body io.ReadCloser) {
		err := Body.Close()
		if err != nil {
			return
		}
	}(response.Body)

	if HandleRequestError(c, response) {
		return
	}

	var full_response string

	for i := 3; i > 0; i-- {
		var continue_info *ContinueInfo
		var response_part string
		response_part, continue_info  = Handler(c, response, token, uid, original_request.Stream)
		full_response += response_part
		if continue_info == nil {
			break
		}
		println("Continuing conversation")
		translated_request.Messages = nil
		translated_request.Action = "continue"
		translated_request.ConversationID = continue_info.ConversationID
		translated_request.ParentMessageID = continue_info.ParentID
		if chat_require.Arkose.Required {
			chatgpt.RenewTokenForRequest(&translated_request)
		}
		response, done = sendConversationRequest(c, translated_request, token, chat_require.Token)

		if done {
			return
		}

		// 以下修复代码来自ChatGPT
		// 在循环内部创建一个局部作用域，并将资源的引用传递给匿名函数，保证资源将在每次迭代结束时被正确释放
		func() {
			defer func(Body io.ReadCloser) {
				err := Body.Close()
				if err != nil {
					return
				}
			}(response.Body)
		}()

		if HandleRequestError(c, response) {
			return
		}
	}

	if c.Writer.Status() != 200 {
		return
	}
	if !original_request.Stream {
		c.JSON(200, newChatCompletion(full_response, translated_request.Model, uid))
	} else {
		c.String(200, "data: [DONE]\n\n")
	}

	chatgpt.UnlockSpecConn(token, uid)
}

func generateId() string {
	id := uuid.NewString()
	id = strings.ReplaceAll(id, "-", "")
	id = base64.StdEncoding.EncodeToString([]byte(id))
	id = reg.ReplaceAllString(id, "")
	return "chatcmpl-" + id
}

func convertAPIRequest(api_request APIRequest, requireArk bool) (chatgpt.CreateConversationRequest) {
	chatgpt_request := NewChatGPTRequest()

	var api_version int
	if strings.HasPrefix(api_request.Model, "gpt-3.5") {
		api_version = 3
		chatgpt_request.Model = "text-davinci-002-render-sha"
	} else if strings.HasPrefix(api_request.Model, "gpt-4") {
		api_version = 4
		chatgpt_request.Model = api_request.Model
		// Cover some models like gpt-4-32k
		if len(api_request.Model) >= 7 && api_request.Model[6] >= 48 && api_request.Model[6] <= 57 {
			chatgpt_request.Model = "gpt-4"
		}
	}
	if requireArk {
		token, err := api.GetArkoseToken(api_version)
		if err == nil {
			chatgpt_request.ArkoseToken = token
		} else {
			fmt.Println("Error getting Arkose token: ", err)
		}
	}
	if api_request.PluginIDs != nil {
		chatgpt_request.PluginIDs = api_request.PluginIDs
		chatgpt_request.Model = "gpt-4-plugins"
	}
	for _, api_message := range api_request.Messages {
		if api_message.Role == "system" {
			api_message.Role = "critic"
		}
		chatgpt_request.AddMessage(api_message.Role, api_message.Content)
	}
	return chatgpt_request
}

func NewChatGPTRequest() chatgpt.CreateConversationRequest {
	disable_history := os.Getenv("ENABLE_HISTORY") != "true"
	return chatgpt.CreateConversationRequest{
		Action:                     "next",
		ParentMessageID:            uuid.NewString(),
		Model:                      "text-davinci-002-render-sha",
		HistoryAndTrainingDisabled: disable_history,
	}
}

func sendConversationRequest(c *gin.Context, request chatgpt.CreateConversationRequest, accessToken string, chat_token string) (*http.Response, bool) {
	jsonBytes, _ := json.Marshal(request)
	req, _ := http.NewRequest(http.MethodPost, api.ChatGPTApiUrlPrefix+"/backend-api/conversation", bytes.NewBuffer(jsonBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", api.UserAgent)
	req.Header.Set(api.AuthorizationHeader, accessToken)
	req.Header.Set("Accept", "text/event-stream")
	if request.ArkoseToken != "" {
		req.Header.Set("Openai-Sentinel-Arkose-Token", request.ArkoseToken)
	}
	if chat_token != "" {
		req.Header.Set("Openai-Sentinel-Chat-Requirements-Token", chat_token)
	}
	if api.PUID != "" {
		req.Header.Set("Cookie", "_puid="+api.PUID+";")
	}
	req.Header.Set("Oai-Language", api.Language)
	if api.OAIDID != "" {
		req.Header.Set("Cookie", req.Header.Get("Cookie")+"oai-did="+api.OAIDID)
		req.Header.Set("Oai-Device-Id", api.OAIDID)
	}
	resp, err := api.Client.Do(req)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, api.ReturnMessage(err.Error()))
		return nil, true
	}

	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusUnauthorized {
			logger.Error(fmt.Sprintf(api.AccountDeactivatedErrorMessage, c.GetString(api.EmailKey)))
		}

		responseMap := make(map[string]interface{})
		json.NewDecoder(resp.Body).Decode(&responseMap)
		c.AbortWithStatusJSON(resp.StatusCode, responseMap)
		return nil, true
	}

	return resp, false
}

func GetImageSource(wg *sync.WaitGroup, url string, prompt string, token string, idx int, imgSource []string) {
	defer wg.Done()
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return
	}
	// Clear cookies
	if api.PUID != "" {
		request.Header.Set("Cookie", "_puid="+api.PUID+";")
	}
	request.Header.Set("Oai-Language", api.Language)
	if api.OAIDID != "" {
		request.Header.Set("Cookie", request.Header.Get("Cookie")+"oai-did="+api.OAIDID)
		request.Header.Set("Oai-Device-Id", api.OAIDID)
	}
	request.Header.Set("User-Agent", api.UserAgent)
	request.Header.Set("Accept", "*/*")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := api.Client.Do(request)
	if err != nil {
		return
	}
	defer response.Body.Close()
	var file_info chatgpt.FileInfo
	err = json.NewDecoder(response.Body).Decode(&file_info)
	if err != nil || file_info.Status != "success" {
		return
	}
	imgSource[idx] = "[![image](" + file_info.DownloadURL + " \"" + prompt + "\")](" + file_info.DownloadURL + ")"
}

func Handler(c *gin.Context, response *http.Response, token string, uuid string, stream bool) (string, *ContinueInfo) {
	max_tokens := false

	// Create a bufio.Reader from the response body
	reader := bufio.NewReader(response.Body)

	// Read the response byte by byte until a newline character is encountered
	if stream {
		// Response content type is text/event-stream
		c.Header("Content-Type", "text/event-stream")
	} else {
		// Response content type is application/json
		c.Header("Content-Type", "application/json")
	}
	var finish_reason string
	var previous_text StringStruct
	var original_response ChatGPTResponse
	var isRole = true
	var waitSource = false
	var isEnd = false
	var imgSource []string
	var isWSS = false
	var convId string
	var respId string
	var wssUrl string
	var connInfo *api.ConnInfo
	var wsSeq int
	var isWSInterrupt bool = false
	var interruptTimer *time.Timer

	if !strings.Contains(response.Header.Get("Content-Type"), "text/event-stream") {
		isWSS = true
		connInfo = chatgpt.FindSpecConn(token, uuid)
		if connInfo.Conn == nil {
			c.JSON(500, gin.H{"error": "No websocket connection"})
			return "", nil
		}
		var wssResponse chatgpt.ChatGPTWSSResponse
		json.NewDecoder(response.Body).Decode(&wssResponse)
		wssUrl = wssResponse.WssUrl
		respId = wssResponse.ResponseId
		convId = wssResponse.ConversationId
	}
	for {
		var line string
		var err error
		if isWSS {
			var messageType int
			var message []byte
			if isWSInterrupt {
				if interruptTimer == nil {
					interruptTimer = time.NewTimer(10 * time.Second)
				}
				select {
				case <-interruptTimer.C:
					c.JSON(500, gin.H{"error": "WS interrupt & new WS timeout"})
					return "", nil
				default:
					goto reader
				}
			}
		reader:
			messageType, message, err = connInfo.Conn.ReadMessage()
			if err != nil {
				connInfo.Ticker.Stop()
				connInfo.Conn.Close()
				connInfo.Conn = nil
				err := chatgpt.CreateWSConn(wssUrl, connInfo, 0)
				if err != nil {
					c.JSON(500, gin.H{"error": err.Error()})
					return "", nil
				}
				isWSInterrupt = true
				connInfo.Conn.WriteMessage(websocket.TextMessage, []byte("{\"type\":\"sequenceAck\",\"sequenceId\":"+strconv.Itoa(wsSeq)+"}"))
				continue
			}
			if messageType == websocket.TextMessage {
				var wssMsgResponse chatgpt.WSSMsgResponse
				json.Unmarshal(message, &wssMsgResponse)
				if wssMsgResponse.Data.ResponseId != respId {
					continue
				}
				wsSeq = wssMsgResponse.SequenceId
				if wsSeq%50 == 0 {
					connInfo.Conn.WriteMessage(websocket.TextMessage, []byte("{\"type\":\"sequenceAck\",\"sequenceId\":"+strconv.Itoa(wsSeq)+"}"))
				}
				base64Body := wssMsgResponse.Data.Body
				bodyByte, err := base64.StdEncoding.DecodeString(base64Body)
				if err != nil {
					continue
				}
				if isWSInterrupt {
					isWSInterrupt = false
					interruptTimer.Stop()
				}
				line = string(bodyByte)
			}
		} else {
			line, err = reader.ReadString('\n')
			if err != nil {
				if err == io.EOF {
					break
				}
				return "", nil
			}
		}
		if len(line) < 6 {
			continue
		}
		// Remove "data: " from the beginning of the line
		line = line[6:]
		// Check if line starts with [DONE]
		if !strings.HasPrefix(line, "[DONE]") {
			// Parse the line as JSON
			err = json.Unmarshal([]byte(line), &original_response)
			if err != nil {
				continue
			}
			if original_response.Error != nil {
				c.JSON(500, gin.H{"error": original_response.Error})
				return "", nil
			}
			if original_response.ConversationID != convId {
				if convId == "" {
					convId = original_response.ConversationID
				} else {
					continue
				}
			}
			if !(original_response.Message.Author.Role == "assistant" || (original_response.Message.Author.Role == "tool" && original_response.Message.Content.ContentType != "text")) || original_response.Message.Content.Parts == nil {
				continue
			}
			if original_response.Message.Metadata.MessageType == "" {
				continue
			}
			if original_response.Message.Metadata.MessageType != "next" && original_response.Message.Metadata.MessageType != "continue" || !strings.HasSuffix(original_response.Message.Content.ContentType, "text") {
				continue
			}
			if original_response.Message.EndTurn != nil {
				if waitSource {
					waitSource = false
				}
				isEnd = true
			}
			if len(original_response.Message.Metadata.Citations) != 0 {
				r := []rune(original_response.Message.Content.Parts[0].(string))
				if waitSource {
					if string(r[len(r)-1:]) == "】" {
						waitSource = false
					} else {
						continue
					}
				}
				offset := 0
				for _, citation := range original_response.Message.Metadata.Citations {
					rl := len(r)
					attr := urlAttrMap[citation.Metadata.URL]
					if attr == "" {
						u, _ := url.Parse(citation.Metadata.URL)
						baseURL := u.Scheme + "://" + u.Host + "/"
						attr = getURLAttribution(token, api.PUID, baseURL)
						if attr != "" {
							urlAttrMap[citation.Metadata.URL] = attr
						}
					}
					original_response.Message.Content.Parts[0] = string(r[:citation.StartIx+offset]) + " ([" + attr + "](" + citation.Metadata.URL + " \"" + citation.Metadata.Title + "\"))" + string(r[citation.EndIx+offset:])
					r = []rune(original_response.Message.Content.Parts[0].(string))
					offset += len(r) - rl
				}
			} else if waitSource {
				continue
			}
			response_string := ""
			if original_response.Message.Recipient != "all" {
				continue
			}
			if original_response.Message.Content.ContentType == "multimodal_text" {
				apiUrl := "https://chat.openai.com/backend-api/files/"
				FILES_REVERSE_PROXY := os.Getenv("FILES_REVERSE_PROXY")
				if FILES_REVERSE_PROXY != "" {
					apiUrl = FILES_REVERSE_PROXY
				}
				imgSource = make([]string, len(original_response.Message.Content.Parts))
				var wg sync.WaitGroup
				for index, part := range original_response.Message.Content.Parts {
					jsonItem, _ := json.Marshal(part)
					var dalle_content chatgpt.DalleContent
					err = json.Unmarshal(jsonItem, &dalle_content)
					if err != nil {
						continue
					}
					url := apiUrl + strings.Split(dalle_content.AssetPointer, "//")[1] + "/download"
					wg.Add(1)
					go GetImageSource(&wg, url, dalle_content.Metadata.Dalle.Prompt, token, index, imgSource)
				}
				wg.Wait()
				translated_response := NewChatCompletionChunk(strings.Join(imgSource, ""))
				if isRole {
					translated_response.Choices[0].Delta.Role = original_response.Message.Author.Role
				}
				response_string = "data: " + translated_response.String() + "\n\n"
			}
			if response_string == "" {
				response_string = ConvertToString(&original_response, &previous_text, isRole)
			}
			if response_string == "" {
				if isEnd {
					goto endProcess
				} else {
					continue
				}
			}
			if response_string == "【" {
				waitSource = true
				continue
			}
			isRole = false
			if stream {
				_, err = c.Writer.WriteString(response_string)
				if err != nil {
					return "", nil
				}
			}
		endProcess:
			// Flush the response writer buffer to ensure that the client receives each line as it's written
			c.Writer.Flush()

			if original_response.Message.Metadata.FinishDetails != nil {
				if original_response.Message.Metadata.FinishDetails.Type == "max_tokens" {
					max_tokens = true
				}
				finish_reason = original_response.Message.Metadata.FinishDetails.Type
			}
			if isEnd {
				if stream {
					final_line := StopChunk(finish_reason)
					c.Writer.WriteString("data: " + final_line.String() + "\n\n")
				}
				break
			}
		}
	}
	if !max_tokens {
		return strings.Join(imgSource, "") + previous_text.Text, nil
	}
	return strings.Join(imgSource, "") + previous_text.Text, &ContinueInfo{
		ConversationID: original_response.ConversationID,
		ParentID:       original_response.Message.ID,
	}

}

var urlAttrMap = make(map[string]string)

type urlAttr struct {
	Url         string `json:"url"`
	Attribution string `json:"attribution"`
}

func getURLAttribution(access_token string, puid string, url string) string {
	request, err := http.NewRequest(http.MethodPost, "https://chat.openai.com/backend-api/attributions", bytes.NewBuffer([]byte(`{"urls":["`+url+`"]}`)))
	if err != nil {
		return ""
	}
	if puid != "" {
		request.Header.Set("Cookie", "_puid="+puid+";")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", api.UserAgent)
	request.Header.Set("Oai-Language", api.Language)
	if access_token != "" {
		request.Header.Set("Authorization", "Bearer "+access_token)
	}
	if err != nil {
		return ""
	}
	response, err := api.Client.Do(request)
	if err != nil {
		return ""
	}
	defer response.Body.Close()
	var attr urlAttr
	err = json.NewDecoder(response.Body).Decode(&attr)
	if err != nil {
		return ""
	}
	return attr.Attribution
}