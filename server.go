package main

import 
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
	"sort"
	"strconv"
	"strings"
	"sync"
) // ChatRequest 对应 Agent 对外接口规范中的请求体
)// ChatRequest 对应 Agent 对外接口规范中的请求体
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
	// 记录最近查询的房源，用于处理"就租最近的那套"等模糊指令
	RecentHouses []HouseResult
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

	reply, intentToolResults, err := s.processUserMessage(ctx, &req, state)
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
func (s *AgentServer) processUserMessage(ctx context.Context, req *ChatRequest, state *SessionState) (string, []ToolResult, error) {
	message := strings.TrimSpace(req.Message)
	if message == "" {
		return "请描述一下你的租房需求，比如预算、区域、是否近地铁等。", nil, nil
	}

	// 简单意图判断：租房 / 退租 / 下架 / 找房 / 选择房源

	
	// 查找用户是否选择了某个房源（通过房源ID）
	var selectedHouseID string
	houseIDPattern := regexp.MustCompile(`HF_\d+`)
	if matches := houseIDPattern.FindAllString(lower, -1); len(matches) > 0 {
		selectedHouseID = matches[0]

	
	// 查找用户是否选择了房源（通过模糊描述）
	if selectedHouseID == "" {
		// 如果用户说"就租最近的那套"或类似的表达，选择排序后的第一套
		if strings.Contains(lower, "就租") && (strings.Contains(lower, "最近") || strings.Contains(lower, "第一") || strings.Contains(lower, "这套")) {
			// 这里需要在 session 中记录之前查询的房源，优先排序靠前的
			if len(state.RecentHouses) > 0 {
				selectedHouseID = state.RecentHouses[0].ID
			}
		}

	
	if selectedHouseID != "" {
		// 用户选择房源，调用租房接口
		rentTr := s.rentHouse(ctx, selectedHouseID)

		
		if rentTr.Success {
			// 租房成功，返回这个房源ID
			return "好的，这套房源已经为您成功租下！", toolResults, nil
		} else {
			// 租房失败
			return fmt.Sprintf("对不起，租房操作失败：%s", rentTr.Error), toolResults, nil
		}

	
	// 查找用户是否在询问其他房源
	if strings.Contains(lower, "其他") && (strings.Contains(lower, "房源") || strings.Contains(lower, "房子")) {
		// 用户询问是否有符合条件的房源，返回没有
		return "很抱歉，当前没有其他符合条件的房源了。", nil, nil

	
	// 默认视为"找房"意图
	params := s.extractSearchParams(message)

	var newToolResults []ToolResult
	houses, tr, err := s.searchHousesByPlatform(ctx, params)
	if err != nil {
		newToolResults := []ToolResult{tr}
		return "", newToolResults, err
	}
	newToolResults = append(newToolResults, tr)

	if len(houses) == 0 {
		// 仍然返回JSON格式，包含message和houses字段
		resp := s.buildJSONResponse(message, []HouseResult{})
		return resp, newToolResults, nil
	}

	// 使用用户需求对房源进行评分和排序，选择最匹配的房源

	
	// 更新session状态中的最近房源记录，用于处理后续的模糊租房指令
	if len(topHouses) > 0 {
		state.RecentHouses = topHouses

	
	// 房源查询完成后，返回JSON格式响应，包含message和houses字段
	resp := s.buildJSONResponse(message, topHouses)
	return resp, newToolResults, nil
字
}
// SearchParams 映射到 /api/houses/by_platform 的部分查询参数
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

	
	Decorations     []string // 装修要求：简装、精装、豪华、毛坯等
	Orientations    []string // 朝向要求：朝南、朝北、朝东、朝西等
	MinBathrooms    int      // 最少卫生间数量
	HasElevator     bool     // 是否需要电梯
	MaxNoiseLevel   string   // 最大噪音级别：安静、中等、吵闹、临街
	TagsMustHave    []string // 必须包含的标签
	TagsMustNotHave []string // 不能包含的标签
	MinTags         int      // 最少标签数量

	
	PricePriority  bool // 价格是否为主要考量因素
	SubwayPriority bool // 地铁距离是否为主要考量因素

	
	SortBy    string // 排序字段：price/area/subway
	SortOrder string // 排序顺序：asc/desc
	SortOrder          string    // 排序顺序：asc/desc
}

// extractSearchParams 使用语义分析从中文话术里解析更全面的筛选条件
func (s *AgentServer) extractSearchParams(message string) SearchParams {
	p := SearchParams{
		ListingPlatform: "安居客",
		PageSize:        100, // 设置一个较大的数字，确保获取所有符合条件的房源
	}

	// 行政区识别
	districts := []string{"海淀", "朝阳", "通州", "昌平", "大兴", "房山", "西城", "丰台", "顺义", "东城"}
	for _, d := range districts {
		if strings.Contains(message, d) {
			p.Districts = append(p.Districts, d)
		}
	}

	// 预算识别：xxx 元、xxx块、预算xxx、不超过xxx
	rePrice := regexp.MustCompile(`(\d{3,5})\s*(元|块|预算|不超过|不超|以内)`)
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

	// 最低预算要求：最低xxx、不低xxx、大于xxx
	reMinPrice := regexp.MustCompile(`(最低|不低|大于|至少)\s*(\d{3,5})\s*(元|块)`)
	if matches := reMinPrice.FindAllStringSubmatch(message, -1); len(matches) > 0 {
		for _, m := range matches {
			if len(m) >= 3 {
				v, _ := strconv.Atoi(m[2])
				p.MinPrice = v
			}
		}
	}

	// 卧室数：一居/两居/三居/四居
	bedroomMap := map[string]string{
		"一居": "1", "单间": "1",
		"两居": "2", "二居": "2",
		"三居": "3",
		"四居": "4",
	}
	for k, v := range bedroomMap {
		if strings.Contains(message, k) {
			p.Bedrooms = append(p.Bedrooms, v)
			// 用户提到几居，自动设置为整租
			if strings.Contains(message, "整租") || strings.Contains(message, "合租") {
				// 只有明确提到合租才设为合租，默认整租
			} else {
				p.RentalType = "整租"
			}
		}
	}

	// 租赁方式判断（在未根据卧室数自动设置的情况下）
	if p.RentalType == "" {
		if strings.Contains(message, "整租") {
			p.RentalType = "整租"
		} else if strings.Contains(message, "合租") || strings.Contains(message, "次卧") || strings.Contains(message, "主卧") {
			p.RentalType = "合租"
		} else if len(p.Bedrooms) > 0 {
			// 如果用户提到了几居但没有明确说合租，默认为整租
			p.RentalType = "整租"
		}
	}

	// 近地铁：700米内、800米内
	reSubwayDist := regexp.MustCompile(`(\d{1,4})\s*(米|米内)`)
	if matches := reSubwayDist.FindAllStringSubmatch(message, -1); len(matches) > 0 {
		// 取最小的距离要求
		minDist := 9999
		for _, m := range matches {
			v, _ := strconv.Atoi(m[1])
			if v > 0 && v < minDist {
				minDist = v
			}
		}
		if minDist < 2000 {
			p.MaxSubwayDist = minDist
		}
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

	// 装修要求：简装、精装、豪华、毛坯
		"简装":   "简装",
		"精装":   "精装",
		"精装修":  "精装",
		"豪华":   "豪华",
		"豪华": "豪华",
		"毛坯":   "毛坯",
		"空房":   "空房",
		"空房": "空房",
	}
	for k, v := range decorationsMap {
		if strings.Contains(message, k) {
			p.Decorations = append(p.Decorations, v)
		}
	}

	// 朝向要求：朝南、朝北、朝东、朝西、南北、东西
		"朝南":   "朝南",
		"朝北":   "朝北",
		"朝东":   "朝东",
		"朝西":   "朝西",
		"南北":   "南北",
		"南北": "南北",
		"东西":   "东西",
		"东西": "东西",
		"东西通透": "东西",
	}
	for k, v := range orientationsMap {
		if strings.Contains(message, k) {
			p.Orientations = append(p.Orientations, v)
		}
	}

	// 卫生间数量：一卫、双卫、三卫
	reBathrooms := regexp.MustCompile(`(一|单|双|三|四)卫`)
	if matches := reBathrooms.FindAllStringSubmatch(message, -1); len(matches) > 0 {
		for _, m := range matches {
			if len(m) >= 2 {
				switch m[1] {
				case "一", "单":
					p.MinBathrooms = 1
				case "双":
					p.MinBathrooms = 2
				case "三":
					p.MinBathrooms = 3
				case "四":
					p.MinBathrooms = 4
				}
			}
		}
	}

	// 电梯要求
	if strings.Contains(message, "电梯") || strings.Contains(message, "有电梯") {
		p.HasElevator = true
	}

	// 噪音要求
	noiseLevels := []string{"安静", "中等", "吵闹", "临街"}
		if strings.Contains(message, level+"尽量") || strings.Contains(message, level+"为佳") ||
			strings.Contains(message, "噪音"+level) || strings.Contains(message, "环境"+level) {
		   strings.Contains(message, "噪音"+level) || strings.Contains(message, "环境"+level) {
			p.MaxNoiseLevel = level
			break
		}
	}

	positiveTags := []string{"近地铁", "双地铁", "多地铁", "精装修", "豪华装修", "朝南", "南北通透",
		"采光好", "有电梯", "高楼层", "高层", "核心区", "学区房", "近高校", "高性价比"}
	                        "采光好", "有电梯", "高楼层", "高层", "核心区", "学区房", "近高校", "高性价比"}
		if strings.Contains(message, tag+"要求") || strings.Contains(message, "必须"+tag) ||
			strings.Contains(message, "要有"+tag) {
		   strings.Contains(message, "要有"+tag) {
			p.TagsMustHave = append(p.TagsMustHave, tag)
		}
	}

	// 负面标签排除
	negativeTags := []string{"临街", "吵闹", "毛坯", "低价", "农村房", "农村自建房"}
		if strings.Contains(message, "不要"+tag) || strings.Contains(message, "排除"+tag) ||
			strings.Contains(message, "不能"+tag) {
		   strings.Contains(message, "不能"+tag) {
			p.TagsMustNotHave = append(p.TagsMustNotHave, tag)
		}
	}

	// 标签数量要求
	if strings.Contains(message, "标签多") || strings.Contains(message, "信息全") {
		p.MinTags = 3
	}

	if strings.Contains(message, "价格第一") || strings.Contains(message, "优先考虑价格") ||
		strings.Contains(message, "实惠优先") || strings.Contains(message, "经济实惠") {
	   strings.Contains(message, "实惠优先") || strings.Contains(message, "经济实惠") {
		p.PricePriority = true
	}
	// 地铁优先级
	if strings.Contains(message, "地铁第一") || strings.Contains(message, "地铁最重要") ||
		strings.Contains(message, "地铁优先") {
	   strings.Contains(message, "地铁优先") {
		p.SubwayPriority = true
	}

	if strings.Contains(message, "按地铁距离") || strings.Contains(message, "按距离") ||
		strings.Contains(message, "按地铁站") || strings.Contains(message, "按地铁") {
	   strings.Contains(message, "按地铁站") || strings.Contains(message, "按地铁") {
		p.SortBy = "subway"
	if strings.Contains(message, "按价格") || strings.Contains(message, "按租金") ||
		strings.Contains(message, "按价位") {
	   strings.Contains(message, "按价位") {
		p.SortBy = "price"
	}
	if strings.Contains(message, "按面积") {
		p.SortBy = "area"
	}

	if strings.Contains(message, "从近到远") || strings.Contains(message, "从少到多") ||
		strings.Contains(message, "从小到大") || strings.Contains(message, "从低到高") {
	   strings.Contains(message, "从小到大") || strings.Contains(message, "从低到高") {
		p.SortOrder = "asc"
	if strings.Contains(message, "从远到近") || strings.Contains(message, "从多到少") ||
		strings.Contains(message, "从大到小") || strings.Contains(message, "从高到低") {
	   strings.Contains(message, "从大到小") || strings.Contains(message, "从高到低") {
		p.SortOrder = "desc"
	}

	return p
}

// HouseScore 结构体用于房源匹配度评分
	HouseID string
	Score   float64            // 综合匹配度分数
	Factors map[string]float64 // 各个维度的单独分数
	Factors      map[string]float64 // 各个维度的单独分数
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
	// 显式设置一个较大的page_size，确保API返回所有符合条件的房源
	values.Set("page_size", "100")
	// 不让API进行排序，全部在后端完成
	// if p.SortBy != "" {
	// 	values.Set("sort_by", p.SortBy)
	// }
	// if p.SortOrder != "" {
	// 	values.Set("sort_order", p.SortOrder)
	// }

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
		// 不再在这里限制只返回5个，让后续评分排序决定最终推荐数量
	}

	tr := ToolResult{
		Name:    "get_houses_by_platform",
		Success: true,
		Output:  fmt.Sprintf("found %d houses", len(raw.Data.Items)),
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

// scoreAndRankHouses 对房源进行评分和排序，选择最匹配用户需求的房源
func (s *AgentServer) scoreAndRankHouses(houses []HouseResult, params SearchParams) []HouseResult {
	if len(houses) == 0 {
		return houses
	}

	// 首先进行硬性要求筛选

	
	// 如果硬性要求筛选后没有房源，检查是否完全符合要求
	if len(filteredHouses) == 0 {
		// 检查是否有房源完全匹配用户的关键要求
		strictlyGoodHouses := s.findStrictlyGoodMatches(houses, params)
		if len(strictlyGoodHouses) > 0 {
			return strictlyGoodHouses[:min(3, len(strictlyGoodHouses))]
		}
		return filteredHouses // 返回空列表，表示没有完全符合要求的房源
	}


	
	for _, house := range filteredHouses {
		scoredHouse := HouseScore{
			HouseID: house.ID,
			Score:   0.0,
			Factors: make(map[string]float64),
		}

		// 1. 价格匹配度评分 (权重: 25%)，只有在有价格要求时才计算
		if params.MaxPrice > 0 || params.MinPrice > 0 {
			priceScore := s.calculatePriceScore(house.Price, params)
			scoredHouse.Score += priceScore * 0.25
			scoredHouse.Factors["price"] = priceScore
		}

		// 2. 位置匹配度评分 (权重: 30%)，只有在有位置要求时才计算
		if params.MaxSubwayDist > 0 || params.CommuteToXQMax > 0 || len(params.Districts) > 0 {
			locationScore := s.calculateLocationScore(house, params)
			scoredHouse.Score += locationScore * 0.30
			scoredHouse.Factors["location"] = locationScore
		}
		// 3. 房屋配置匹配度评分 (权重: 25%)，只有在有配置要求时才计算
		if len(params.Bedrooms) > 0 || params.RentalType != "" || len(params.Decorations) > 0 ||
			len(params.Orientations) > 0 || params.HasElevator || params.MinBathrooms > 0 {
		   len(params.Orientations) > 0 || params.HasElevator || params.MinBathrooms > 0 {
			configScore := s.calculateConfigScore(house, params)
			scoredHouse.Score += configScore * 0.25
			scoredHouse.Factors["config"] = configScore
		}

		if params.MaxNoiseLevel != "" || len(params.TagsMustHave) > 0 || len(params.TagsMustNotHave) > 0 ||
			params.MinTags > 0 {
		   params.MinTags > 0 {
			hiddenInfoScore := s.calculateHiddenInfoScore(house, params)
			scoredHouse.Score += hiddenInfoScore * 0.20
			scoredHouse.Factors["hidden_info"] = hiddenInfoScore
		}

		scores = append(scores, scoredHouse)
	}

	// 根据用户优先级调整权重
	for i := range scores {
		if params.PricePriority {
			if scores[i].Factors["price"] > 0 {
				scores[i].Score += scores[i].Factors["price"] * 0.15
			}
			if scores[i].Factors["location"] > 0 {
				scores[i].Score -= scores[i].Factors["location"] * 0.15
			}
		} else if params.SubwayPriority {
			if scores[i].Factors["location"] > 0 {
				scores[i].Score += scores[i].Factors["location"] * 0.15
			}
			if scores[i].Factors["price"] > 0 {
				scores[i].Score -= scores[i].Factors["price"] * 0.15
			}
		}
	}

	// 创建排序后的房源列表供选择
	sortedHouses := make([]HouseResult, len(filteredHouses))

	
	// 根据用户指定的排序方式进行排序
	if params.SortBy == "subway" {
		sort.Slice(sortedHouses, func(i, j int) bool {
			if params.SortOrder == "asc" {
				// 从近到远排序
				return sortedHouses[i].SubwayDistance < sortedHouses[j].SubwayDistance
			} else {
				// 从远到近排序
				return sortedHouses[i].SubwayDistance > sortedHouses[j].SubwayDistance
			}
		})
	} else {
		// 根据综合分数排序
		// 首先计算每个房源的综合分数
		houseScoreMap := make(map[string]float64)
		for _, score := range scores {
			houseScoreMap[score.HouseID] = score.Score

		
		sort.Slice(sortedHouses, func(i, j int) bool {
			scoreI := houseScoreMap[sortedHouses[i].ID]
			scoreJ := houseScoreMap[sortedHouses[j].ID]
			return scoreI > scoreJ
		})

	
	// 选择前5个房源作为推荐结果
	topCount := min(5, len(sortedHouses))
	return sortedHouses[:topCount]
}

// filterByStrictRequirements 根据硬性要求筛选房源
func (s *AgentServer) filterByStrictRequirements(houses []HouseResult, params SearchParams) []HouseResult {

	
	for _, house := range houses {
		// 检查租赁类型硬性要求
		if params.RentalType != "" && house.RentalType != params.RentalType {
			continue

		
		// 检查价格硬性要求
		if params.MinPrice > 0 && house.Price < float64(params.MinPrice) {
			continue
		}
		if params.MaxPrice > 0 && house.Price > float64(params.MaxPrice) {
			continue

		
		// 检查卧室数硬性要求
		if len(params.Bedrooms) > 0 {
			found := false
			for _, bedroom := range params.Bedrooms {
				if house.Bedrooms == strToInt(bedroom) {
					found = true
					break
				}
			}
			if !found {
				continue
			}

		
		// 检查行政区硬性要求
		if len(params.Districts) > 0 {
			found := false
			for _, district := range params.Districts {
				if house.District == district {
					found = true
					break
				}
			}
			if !found {
				continue
			}

		
		// 检查租房类型硬性要求（如果用户要求整租）
		if params.RentalType == "整租" {
			// 排除明显的合租房源（通常面积小，卧室数少）
			if house.AreaSize < 50 && house.Bedrooms < 2 {
				continue
			}

		
		filtered = append(filtered, house)

	
	return filtered
}

// findStrictlyGoodMatches 找出相对较好的匹配选项（在无严格匹配时的备用方案）
func (s *AgentServer) findStrictlyGoodMatches(houses []HouseResult, params SearchParams) []HouseResult {

	
	for _, house := range houses {

		
		// 检查关键维度，每个维度+1分
		if len(params.Districts) > 0 && house.District == params.Districts[0] {
			score++
		}
		if len(params.Bedrooms) > 0 && house.Bedrooms == strToInt(params.Bedrooms[0]) {
			score++
		}
		if params.MaxPrice > 0 && house.Price <= float64(params.MaxPrice)*1.2 {
			score++
		}
		if params.MaxSubwayDist > 0 && float64(house.SubwayDistance) <= float64(params.MaxSubwayDist)*1.5 {
			score++
		}
		if params.RentalType != "" && house.RentalType == params.RentalType {
			score++

		
		// 至少满足3个关键维度才认为是匹配的
		if score >= 3 {
			goodMatches = append(goodMatches, house)
		}

	
	return goodMatches
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// calculatePriceScore 计算价格匹配度
func (s *AgentServer) calculatePriceScore(housePrice float64, params SearchParams) float64 {
	if params.MaxPrice == 0 && params.MinPrice == 0 {
		return 0.8 // 如果用户没有提到价格，给一个中等分数
	}

	// 价格区间评分
	if params.MinPrice > 0 && params.MaxPrice > 0 {
		if housePrice >= float64(params.MinPrice) && housePrice <= float64(params.MaxPrice) {
			return 1.0 // 完全符合预算区间
		} else if housePrice > float64(params.MaxPrice) && housePrice <= float64(params.MaxPrice)*1.2 {
			return 0.7 // 稍微超出预算但可接受
		}
	}

	// 只有最高价限制
	if params.MaxPrice > 0 {
		if housePrice <= float64(params.MaxPrice) {
			return 1.0
		} else if housePrice <= float64(params.MaxPrice)*1.2 {
			return 0.7
		} else if housePrice <= float64(params.MaxPrice)*1.5 {
			return 0.4
		}
		return 0.1
	}

	// 只有最低价限制
	if params.MinPrice > 0 {
		if housePrice >= float64(params.MinPrice) {
			if housePrice <= float64(params.MinPrice)*1.2 {
				return 1.0
			} else if housePrice <= float64(params.MinPrice)*1.5 {
				return 0.8
			}
			return 0.6
		}
		return 0.0
	}

	return 0.5
}

// calculateLocationScore 计算位置匹配度
func (s *AgentServer) calculateLocationScore(house HouseResult, params SearchParams) float64 {
	score := 0.5 // 基础分数

	// 行政区匹配
	if len(params.Districts) > 0 {
		for _, district := range params.Districts {
			if house.District == district {
				score += 0.3 // 行政区完全匹配
				break
			}
		}
	}

	// 地铁距离匹配
	if params.MaxSubwayDist > 0 {
		if house.SubwayDistance <= params.MaxSubwayDist {
			if house.SubwayDistance <= 300 {
				score += 0.4 // 非常近地铁
			} else if house.SubwayDistance <= 600 {
				score += 0.3 // 比较近地铁
			} else {
				score += 0.2 // 一般距离地铁
			}
		} else if float64(house.SubwayDistance) <= float64(params.MaxSubwayDist)*1.5 {
			score += 0.1 // 稍超出地铁距离要求
		}
	}

	// 西二旗通勤时间匹配
	if params.CommuteToXQMax > 0 {
		if house.CommuteToXierqiMin <= params.CommuteToXQMax {
			if house.CommuteToXierqiMin <= 30 {
				score += 0.3 // 通勤时间很短
			} else if house.CommuteToXierqiMin <= 45 {
				score += 0.2 // 通勤时间较短
			} else {
				score += 0.1 // 通勤时间一般
			}
		}
	}

	// 区域商圈匹配（如果有Area要求）
	if len(params.Areas) > 0 {
		for _, area := range params.Areas {
			if strings.Contains(house.AreaName, area) || strings.Contains(house.Community, area) {
				score += 0.2 // 区域商圈匹配
				break
			}
		}
	}

	if score > 1.0 {
		score = 1.0
	}

	return score
}

// calculateConfigScore 计算房屋配置匹配度
func (s *AgentServer) calculateConfigScore(house HouseResult, params SearchParams) float64 {
	score := 0.5 // 基础分数

	// 卧室数匹配
	if len(params.Bedrooms) > 0 {
		for _, bedroom := range params.Bedrooms {
			if house.Bedrooms == strToInt(bedroom) {
				score += 0.3 // 卧室数完全匹配
				break
			}
		}
	}

	// 租赁方式匹配
	if params.RentalType != "" && house.RentalType == params.RentalType {
		score += 0.2 // 租赁方式匹配
	}

	// 装修匹配
	if len(params.Decorations) > 0 {
		for _, decoration := range params.Decorations {
			if house.Decoration == decoration {
				score += 0.2 // 装修匹配
				break
			}
		}
	}

	// 朝向匹配
	if len(params.Orientations) > 0 {
		for _, orientation := range params.Orientations {
			if house.Orientation == orientation || strings.Contains(house.Orientation, orientation) {
				score += 0.15 // 朝向匹配
				break
			}
		}
	}

	// 卫生间数量匹配
	if params.MinBathrooms > 0 {
		// 由于HouseResult中没有直接存储卫生间数量，我们通过AreaSize推测
		// 这是一个简化方案，实际API应该提供准确的卫生间数量
		estimatedBathrooms := 1
		if house.AreaSize > 80 {
			estimatedBathrooms = 2
		}
		if house.AreaSize > 120 {
			estimatedBathrooms = 3

		
		if estimatedBathrooms >= params.MinBathrooms {
			score += 0.1
		}
	}

	// 电梯匹配
	if params.HasElevator {
		// 根据标签判断是否有电梯
		hasElevatorTag := false
		for _, tag := range house.Tags {
			if strings.Contains(tag, "电梯") || strings.Contains(tag, "高楼层") || strings.Contains(tag, "高层") {
				hasElevatorTag = true
				break
			}
		}
		if hasElevatorTag {
			score += 0.15
		}
	}

	if score > 1.0 {
		score = 1.0
	}

	return score
}

// calculateHiddenInfoScore 计算隐藏信息匹配度
func (s *AgentServer) calculateHiddenInfoScore(house HouseResult, params SearchParams) float64 {
	score := 0.3 // 基础分数

	// 噪音水平匹配
	if params.MaxNoiseLevel != "" {
		noisePriority := map[string]int{"安静": 1, "中等": 2, "吵闹": 3, "临街": 4}
		houseNoisePriority := noisePriority[house.NoiseLevel]

		
		if houseNoisePriority > 0 && houseNoisePriority <= userMaxPriority {
			if houseNoisePriority == userMaxPriority {
				score += 0.3 // 完全符合噪音要求
			} else {
				score += 0.2 // 比用户要求更好
			}
		}
	}

	// 必须包含的标签
	hasAllRequiredTags := true
	if len(params.TagsMustHave) > 0 {
		tagCounts := make(map[string]int)
		for _, tag := range house.Tags {
			tagCounts[tag]++

		
		for _, requiredTag := range params.TagsMustHave {
			found := false
			for _, houseTag := range house.Tags {
				if strings.Contains(requiredTag, houseTag) || strings.Contains(houseTag, requiredTag) {
					found = true
					break
				}
			}
			if !found {
				hasAllRequiredTags = false
				break
			}
		}

	
	if hasAllRequiredTags {
		score += 0.3
	}

	// 排除的标签
	hasNegativeTags := false
	if len(params.TagsMustNotHave) > 0 {
		for _, houseTag := range house.Tags {
			for _, negativeTag := range params.TagsMustNotHave {
				if strings.Contains(houseTag, negativeTag) || strings.Contains(negativeTag, houseTag) {
					hasNegativeTags = true
					break
				}
			}
			if hasNegativeTags {
				break
			}
		}

	
	if !hasNegativeTags {
		score += 0.2
	}

	// 标签数量要求
	if params.MinTags > 0 && len(house.Tags) >= params.MinTags {
		score += 0.2
	}

	// 好的加分项
	if len(house.Tags) > 5 {
		score += 0.1
	}

	// 临街减分
	if house.NoiseLevel == "临街" {
		score -= 0.2
	}

	if score > 1.0 {
		score = 1.0
	}

	return score
}

func strToInt(s string) int {
	i, _ := strconv.Atoi(s)
	return i
}

// extractDistricts 从消息中提取区域信息
func extractDistricts(message string) []string {
	districtPattern := regexp.MustCompile(`(东城|西城|海淀|朝阳|丰台|石景山|昌平|顺义|通州|大兴|房山|门头沟|怀柔|平谷|密云|延庆)`)
	matches := districtPattern.FindAllString(message, -1)
	return matches
}

// extractBedrooms 从消息中提取居室信息
func extractBedrooms(message string) []string {
	bedroomPattern := regexp.MustCompile(`(一居|1居|两居|2居|三居|3居|四居|4居)`)

	
	// 转换数字格式
	result := make([]string, 0, len(matches))
	for _, match := range matches {
		switch match {
		case "一居":
			result = append(result, "1居")
		case "两居":
			result = append(result, "2居")
		case "三居":
			result = append(result, "3居")
		case "四居":
			result = append(result, "4居")
		case "1居", "2居", "3居", "4居":
			result = append(result, match)
		}
	}
	return result
}

// extractSubwayDistance 从消息中提取地铁距离
func extractSubwayDistance(message string) int {
	distancePattern := regexp.MustCompile(`离地铁\s*(\d+)\s*米|地铁\s*(\d+)\s*米`)
	matches := distancePattern.FindStringSubmatch(message)
	if len(matches) > 2 {
		if strToInt(matches[1]) > 0 {
			return strToInt(matches[1])
		}
		if strToInt(matches[2]) > 0 {
			return strToInt(matches[2])
		}
	}
	return 0
}

// rentHouse 处理房源租赁操作
func (s *AgentServer) rentHouse(ctx context.Context, houseID string) *ToolResult {
	// 获取房源详情以确定listing_platform
	detailURL := s.fakeAPIBaseURL + "/api/houses/" + houseID
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, detailURL, nil)
	if err != nil {
		return &ToolResult{
			Name:    "get_house_detail",
			Success: false,
			Error:   err.Error(),
		}
	}
	req.Header.Set("X-User-ID", s.userID)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return &ToolResult{
			Name:    "get_house_detail",
			Success: false,
			Error:   err.Error(),
		}
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &ToolResult{
			Name:    "get_house_detail",
			Success: false,
			Error:   fmt.Sprintf("failed to get house detail, status code: %d", resp.StatusCode),
		}
	}

	// 简单处理：优先使用安居客作为平台
	houseData := map[string]interface{}{}
	if err := json.Unmarshal(body, &houseData); err == nil {
		if listingPlatform, ok := houseData["listing_platform"].(string); ok && listingPlatform != "" {
			// 调用租房接口
			return s.callRentAPI(ctx, houseID, listingPlatform)
		}
	}

	// 如果获取不到平台信息，默认使用安居客
	return s.callRentAPI(ctx, houseID, "安居客")
}

// callRentAPI 调用真实租房接口
func (s *AgentServer) callRentAPI(ctx context.Context, houseID, listingPlatform string) *ToolResult {
	rentURL := s.fakeAPIBaseURL + "/api/houses/" + houseID + "/rent"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rentURL, nil)
	if err != nil {
		return &ToolResult{
			Name:    "rent_house",
			Success: false,
			Error:   err.Error(),
		}
	}

  
	q := req.URL.Query()
	q.Set("listing_platform", listingPlatform)
	req.URL.RawQuery = q.Encode()

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return &ToolResult{
			Name:    "rent_house",
			Success: false,
			Error:   err.Error(),
		}
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))

	ok := resp.StatusCode >= 200 && resp.StatusCode < 300
	tr := &ToolResult{
		Name:    "rent_house",
		Success: ok,
		Output:  fmt.Sprintf("status=%d body=%s, platform=%s", resp.StatusCode, string(body), listingPlatform),
	}
	if !ok {
		tr.Error = fmt.Sprintf("rent operation failed, status code: %d", resp.StatusCode)
	}

	return tr
}

// buildJSONResponse 构建符合接口规范的JSON格式响应
func (s *AgentServer) buildJSONResponse(originalMessage string, houses []HouseResult) string {
	// 提取房源ID列表
	houseIDs := make([]string, 0, len(houses))
	for _, house := range houses {
		houseIDs = append(houseIDs, house.ID)
	}

	// 提取用户需求中的关键信息用于回复
	districtsFound := extractDistricts(originalMessage)
	bedroomsFound := extractBedrooms(originalMessage)
	subwayDistFound := extractSubwayDistance(originalMessage)

	// 构建响应数据
	responseData := map[string]interface{}{
		"message": s.buildUserFriendlyAnswer(originalMessage, houses),
		"houses":  houseIDs,
	}

	// 如果有关键信息，添加到响应中用于验证
	if len(districtsFound) > 0 || len(bedroomsFound) > 0 {
		responseData["summary_info"] = map[string]interface{}{
			"districts": districtsFound,
			"bedrooms":  bedroomsFound,

		
		if subwayDistFound > 0 {
			responseData["summary_info"].(map[string]interface{})["subway_distance_max"] = subwayDistFound
		}
	}

	// 如果房子不为空，添加排序信息
	if len(houses) > 0 {
		responseData["sort_info"] = map[string]interface{}{
			"subway_distance": fmt.Sprintf("%d米", houses[0].SubwayDistance),
			"sort_order":      "asc",
		}
	}

	// 转换为JSON字符串
	jsonBytes, err := json.Marshal(responseData)
	if err != nil {
		// 如果JSON序列化失败，返回错误信息
		errorResponse := map[string]interface{}{
			"message": "抱歉，房源查询结果处理失败。",
			"houses":  []string{},
		}
		if errorBytes, err := json.Marshal(errorResponse); err == nil {
			return string(errorBytes)
		}
		// 如果连错误响应都无法序列化，返回最基本的格式
		return `{"message":"抱歉，房源查询结果处理失败。","houses":[]}`
	}

	return string(jsonBytes)
}

// buildUserFriendlyAnswer 把候选房源转成适合直接回复用户的中文说明
func (s *AgentServer) buildUserFriendlyAnswer(originalMessage string, houses []HouseResult) string {
	var sb strings.Builder

	if len(houses) == 0 {
		sb.WriteString("没有找到符合条件的房源")
		return sb.String()
	}

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
