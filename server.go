package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ChatRequest 对应 Agent 对外接口规范中的请求体
type ChatRequest struct {
	ModelIP   string `json:"model_ip"`
	SessionID string `json:"session_id"`
	Message   string `json:"message"`
}

// ToolResult 用于在响应中记录一次外部工具 / API 调用情况
type ToolResult struct {
	Name    string `json:"name"`
	Success bool   `json:"success"`
	Output  string `json:"output,omitempty"`
	Error   string `json:"error,omitempty"`
}

// ChatResponse 对应 Agent 对外接口规范中的响应体
type ChatResponse struct {
	SessionID   string       `json:"session_id"`
	Response    string       `json:"response"`
	Status      string       `json:"status"`
	ToolResults []ToolResult `json:"tool_results,omitempty"`
	Timestamp   int64        `json:"timestamp"`
	DurationMs  int64        `json:"duration_ms"`
}

// SessionState 维护每个 session 的简单上下文
type SessionState struct {
	ID              string
	LastMessageTime time.Time
	// 标记当前 session 是否已经重置过房源数据
	HouseDataInited bool
}

// AgentServer 是整个 Agent 的核心结构
type AgentServer struct {
	httpClient     *http.Client
	userID         string
	fakeAPIBaseURL string

	mu       sync.Mutex
	sessions map[string]*SessionState
}

func NewAgentServer() *AgentServer {
	userID := os.Getenv("FAKE_APP_USER_ID")
	if userID == "" {
		// 为了防止误用，这里仍然要求显式配置
		log.Println("warning: FAKE_APP_USER_ID not set, defaulting to demo-user (请在生产环境中配置真实工号)")
		userID = "h00848321"
	}

	baseURL := os.Getenv("FAKE_APP_BASE_URL")
	if baseURL == "" {
		baseURL = "http://7.221.6.201:8080"
	}

	return &AgentServer{
		httpClient: &http.Client{
			Timeout: 8 * time.Second,
		},
		userID:         userID,
		fakeAPIBaseURL: strings.TrimRight(baseURL, "/"),
		sessions:       make(map[string]*SessionState),
	}
}

// HandleChat 处理 /api/v1/chat 请求
func (s *AgentServer) HandleChat(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.SessionID == "" || req.Message == "" {
		http.Error(w, "session_id and message are required", http.StatusBadRequest)
		return
	}

	ctx := r.Context()

	state := s.getOrCreateSession(req.SessionID)

	toolResults := make([]ToolResult, 0, 4)

	// 新 session 先做一次房源初始化，保证评测幂等
	if !state.HouseDataInited {
		if tr := s.initHouseData(ctx); tr != nil {
			toolResults = append(toolResults, *tr)
			if tr.Success {
				state.HouseDataInited = true
			}
		}
	}

	reply, intentToolResults, err := s.processUserMessage(ctx, &req)
	toolResults = append(toolResults, intentToolResults...)

	duration := time.Since(start)

	resp := ChatResponse{
		SessionID:   req.SessionID,
		Response:    reply,
		Status:      "success",
		ToolResults: toolResults,
		Timestamp:   time.Now().Unix(),
		DurationMs:  duration.Milliseconds(),
	}

	if err != nil {
		resp.Status = "error"
		resp.Response = fmt.Sprintf("抱歉，本次请求处理失败：%v", err)
	}

	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(resp); err != nil {
		log.Printf("encode response error: %v", err)
	}
}

func (s *AgentServer) getOrCreateSession(id string) *SessionState {
	s.mu.Lock()
	defer s.mu.Unlock()

	if st, ok := s.sessions[id]; ok {
		st.LastMessageTime = time.Now()
		return st
	}

	st := &SessionState{
		ID:              id,
		LastMessageTime: time.Now(),
	}
	s.sessions[id] = st
	return st
}

// initHouseData 调用 /api/houses/init 重置房源状态
func (s *AgentServer) initHouseData(ctx context.Context) *ToolResult {
	url := s.fakeAPIBaseURL + "/api/houses/init"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return &ToolResult{
			Name:    "init_houses",
			Success: false,
			Error:   err.Error(),
		}
	}
	req.Header.Set("X-User-ID", s.userID)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return &ToolResult{
			Name:    "init_houses",
			Success: false,
			Error:   err.Error(),
		}
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))

	ok := resp.StatusCode >= 200 && resp.StatusCode < 300
	tr := &ToolResult{
		Name:    "init_houses",
		Success: ok,
		Output:  fmt.Sprintf("status=%d body=%s", resp.StatusCode, string(body)),
	}
	if !ok {
		tr.Error = fmt.Sprintf("unexpected status code %d", resp.StatusCode)
	}
	return tr
}

// processUserMessage 根据用户自然语言做需求解析、房源查询与结果组织
func (s *AgentServer) processUserMessage(ctx context.Context, req *ChatRequest) (string, []ToolResult, error) {
	message := strings.TrimSpace(req.Message)
	if message == "" {
		return "请描述一下你的租房需求，比如预算、区域、是否近地铁等。", nil, nil
	}

	// 简单意图判断：租房 / 退租 / 下架 / 找房
	lower := strings.ToLower(message)
	if strings.Contains(lower, "退租") {
		return "目前示例 Agent 仅实现智能找房与推荐流程，退租/下架等操作可在此基础上扩展实现。", nil, nil
	}

	// 默认视为“找房”意图
	params := s.extractSearchParams(message)

	houses, tr, err := s.searchHousesByPlatform(ctx, params)
	toolResults := []ToolResult{tr}
	if err != nil {
		return "", toolResults, err
	}

	if len(houses) == 0 {
		return "根据你目前提供的条件，没有找到合适的房源，你可以尝试放宽预算、通勤时间或调整目标区域。", toolResults, nil
	}

	resp := s.buildUserFriendlyAnswer(message, houses)
	return resp, toolResults, nil
}

// SearchParams 映射到 /api/houses/by_platform 的部分查询参数
type SearchParams struct {
	ListingPlatform string
	Districts       []string
	Areas           []string
	MinPrice        int
	MaxPrice        int
	Bedrooms        []string
	RentalType      string
	MaxSubwayDist   int
	CommuteToXQMax  int
	PageSize        int
}

// extractSearchParams 使用非常简单的规则从中文话术里解析出筛选条件
func (s *AgentServer) extractSearchParams(message string) SearchParams {
	p := SearchParams{
		ListingPlatform: "安居客",
		PageSize:        20,
	}

	// 行政区识别
	districts := []string{"海淀", "朝阳", "通州", "昌平", "大兴", "房山", "西城", "丰台", "顺义", "东城"}
	for _, d := range districts {
		if strings.Contains(message, d) {
			p.Districts = append(p.Districts, d)
		}
	}

	// 预算识别：xxx 元、xxx块
	rePrice := regexp.MustCompile(`(\d{3,5})\s*(元|块)`)
	matches := rePrice.FindAllStringSubmatch(message, -1)
	if len(matches) > 0 {
		// 取最大值作为预算上限
		max := 0
		for _, m := range matches {
			v, _ := strconv.Atoi(m[1])
			if v > max {
				max = v
			}
		}
		p.MaxPrice = max
	}

	// 卧室数：一居/两居/三居/四居
	bedroomMap := map[string]string{
		"一居": "1",
		"两居": "2",
		"二居": "2",
		"三居": "3",
		"四居": "4",
	}
	for k, v := range bedroomMap {
		if strings.Contains(message, k) {
			p.Bedrooms = append(p.Bedrooms, v)
		}
	}

	// 租赁方式
	if strings.Contains(message, "整租") {
		p.RentalType = "整租"
	} else if strings.Contains(message, "合租") || strings.Contains(message, "次卧") || strings.Contains(message, "主卧") {
		p.RentalType = "合租"
	}

	// 近地铁
	if strings.Contains(message, "近地铁") || strings.Contains(message, "地铁方便") {
		p.MaxSubwayDist = 800
	}

	// 到西二旗通勤时间上限：xx分钟内到西二旗
	if strings.Contains(message, "西二旗") || strings.Contains(message, "xierqi") {
		reCommute := regexp.MustCompile(`(\d{1,3})\s*分钟`)
		if m := reCommute.FindStringSubmatch(message); len(m) == 2 {
			if v, err := strconv.Atoi(m[1]); err == nil {
				p.CommuteToXQMax = v
			}
		}
	}

	return p
}

// HouseResult 只保留生成文案需要的字段
type HouseResult struct {
	ID                 string  `json:"house_id"`
	Title              string  `json:"title"`
	Community          string  `json:"community"`
	District           string  `json:"district"`
	AreaName           string  `json:"area"`
	Price              float64 `json:"price"`
	Bedrooms           int     `json:"bedrooms"`
	AreaSize           float64 `json:"area_sqm"`
	RentalType         string  `json:"rental_type"`
	SubwayStation      string  `json:"subway_station"`
	SubwayDistance     int     `json:"subway_distance"`
	CommuteToXierqiMin int     `json:"commute_to_xierqi"`
	Decoration         string  `json:"decoration"`
	Orientation        string  `json:"orientation"`
	NoiseLevel         string  `json:"noise_level"`
	Tags               []string
}

// searchHousesByPlatform 调用 /api/houses/by_platform 查询房源
func (s *AgentServer) searchHousesByPlatform(ctx context.Context, p SearchParams) ([]HouseResult, ToolResult, error) {
	values := url.Values{}
	if p.ListingPlatform != "" {
		values.Set("listing_platform", p.ListingPlatform)
	}
	if len(p.Districts) > 0 {
		values.Set("district", strings.Join(p.Districts, ","))
	}
	if len(p.Areas) > 0 {
		values.Set("area", strings.Join(p.Areas, ","))
	}
	if p.MinPrice > 0 {
		values.Set("min_price", strconv.Itoa(p.MinPrice))
	}
	if p.MaxPrice > 0 {
		values.Set("max_price", strconv.Itoa(p.MaxPrice))
	}
	if len(p.Bedrooms) > 0 {
		values.Set("bedrooms", strings.Join(p.Bedrooms, ","))
	}
	if p.RentalType != "" {
		values.Set("rental_type", p.RentalType)
	}
	if p.MaxSubwayDist > 0 {
		values.Set("max_subway_dist", strconv.Itoa(p.MaxSubwayDist))
	}
	if p.CommuteToXQMax > 0 {
		values.Set("commute_to_xierqi_max", strconv.Itoa(p.CommuteToXQMax))
	}
	if p.PageSize > 0 {
		values.Set("page_size", strconv.Itoa(p.PageSize))
	}

	fullURL := s.fakeAPIBaseURL + "/api/houses/by_platform"
	if q := values.Encode(); q != "" {
		fullURL = fullURL + "?" + q
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return nil, ToolResult{
			Name:    "get_houses_by_platform",
			Success: false,
			Error:   err.Error(),
		}, err
	}
	httpReq.Header.Set("X-User-ID", s.userID)

	resp, err := s.httpClient.Do(httpReq)
	if err != nil {
		return nil, ToolResult{
			Name:    "get_houses_by_platform",
			Success: false,
			Error:   err.Error(),
		}, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, ToolResult{
			Name:    "get_houses_by_platform",
			Success: false,
			Error:   "read response body error: " + err.Error(),
		}, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		err = fmt.Errorf("unexpected status %d from fake app api", resp.StatusCode)
		return nil, ToolResult{
			Name:    "get_houses_by_platform",
			Success: false,
			Output:  string(body),
			Error:   err.Error(),
		}, err
	}

	var raw struct {
		Data struct {
			Items []map[string]any `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, ToolResult{
			Name:    "get_houses_by_platform",
			Success: false,
			Output:  string(body),
			Error:   "decode response error: " + err.Error(),
		}, err
	}

	results := make([]HouseResult, 0, len(raw.Data.Items))
	for _, it := range raw.Data.Items {
		h := HouseResult{
			ID:                 asString(it["house_id"]),
			Title:              asString(it["title"]),
			Community:          asString(it["community"]),
			District:           asString(it["district"]),
			AreaName:           asString(it["area"]),
			Price:              asFloat(it["price"]),
			Bedrooms:           int(asFloat(it["bedrooms"])),
			AreaSize:           asFloat(it["area"]),
			RentalType:         asString(it["rental_type"]),
			SubwayStation:      asString(it["subway_station"]),
			SubwayDistance:     int(asFloat(it["subway_distance"])),
			CommuteToXierqiMin: int(asFloat(it["commute_to_xierqi"])),
			Decoration:         asString(it["decoration"]),
			Orientation:        asString(it["orientation"]),
			NoiseLevel:         asString(it["noise_level"]),
			Tags:               asStringSlice(it["tags"]),
		}
		results = append(results, h)
		if len(results) >= 5 {
			break
		}
	}

	tr := ToolResult{
		Name:    "get_houses_by_platform",
		Success: true,
		Output:  fmt.Sprintf("found %d houses, returned %d", len(raw.Data.Items), len(results)),
	}
	return results, tr, nil
}

func asString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case fmt.Stringer:
		return x.String()
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	default:
		return ""
	}
}

func asFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case float32:
		return float64(x)
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case json.Number:
		f, _ := x.Float64()
		return f
	default:
		return 0
	}
}

func asStringSlice(v any) []string {
	switch x := v.(type) {
	case []any:
		res := make([]string, 0, len(x))
		for _, item := range x {
			res = append(res, asString(item))
		}
		return res
	case []string:
		return x
	default:
		return nil
	}
}

// buildUserFriendlyAnswer 把候选房源转成适合直接回复用户的中文说明
func (s *AgentServer) buildUserFriendlyAnswer(originalMessage string, houses []HouseResult) string {
	var sb strings.Builder

	sb.WriteString("我根据你的需求，帮你筛选出了")
	sb.WriteString(strconv.Itoa(len(houses)))
	sb.WriteString("套比较匹配的房源（最多展示 5 套）：\n")

	for i, h := range houses {
		sb.WriteString("\n")
		sb.WriteString(strconv.Itoa(i + 1))
		sb.WriteString("）")
		if h.Title != "" {
			sb.WriteString(h.Title)
		} else {
			sb.WriteString(h.Community)
		}
		if h.District != "" || h.AreaName != "" {
			sb.WriteString("（")
			if h.District != "" {
				sb.WriteString(h.District)
			}
			if h.AreaName != "" {
				if h.District != "" {
					sb.WriteString("·")
				}
				sb.WriteString(h.AreaName)
			}
			sb.WriteString("）")
		}
		if h.RentalType != "" {
			sb.WriteString("，")
			sb.WriteString(h.RentalType)
		}
		if h.Bedrooms > 0 {
			sb.WriteString(strconv.Itoa(h.Bedrooms))
			sb.WriteString("居")
		}
		if h.AreaSize > 0 {
			sb.WriteString(fmt.Sprintf("，约 %.0f ㎡", h.AreaSize))
		}
		if h.Price > 0 {
			sb.WriteString(fmt.Sprintf("，租金 %.0f 元/月", h.Price))
		}
		if h.SubwayStation != "" {
			sb.WriteString("，靠近地铁站「")
			sb.WriteString(h.SubwayStation)
			sb.WriteString("」")
			if h.SubwayDistance > 0 {
				sb.WriteString(fmt.Sprintf("，步行约 %d 米", h.SubwayDistance))
			}
		}
		if h.CommuteToXierqiMin > 0 {
			sb.WriteString(fmt.Sprintf("，到西二旗通勤约 %d 分钟", h.CommuteToXierqiMin))
		}
		if h.Decoration != "" {
			sb.WriteString("，装修：")
			sb.WriteString(h.Decoration)
		}
		if h.Orientation != "" {
			sb.WriteString("，朝向：")
			sb.WriteString(h.Orientation)
		}
		if h.NoiseLevel != "" {
			sb.WriteString("，噪音水平：")
			sb.WriteString(h.NoiseLevel)
		}
		if len(h.Tags) > 0 {
			sb.WriteString("，标签：")
			if len(h.Tags) > 5 {
				sb.WriteString(strings.Join(h.Tags[:5], "、"))
			} else {
				sb.WriteString(strings.Join(h.Tags, "、"))
			}
		}
		sb.WriteString("。")
	}

	sb.WriteString("\n\n如果你有更具体的要求（例如「预算再低一点」或「必须步行 10 分钟内到地铁」），可以继续告诉我，我可以在此基础上再帮你缩小范围。")
	return sb.String()
}
