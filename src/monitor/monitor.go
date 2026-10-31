package monitor

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/bililive-go/bililive-go/src/instance"
	"github.com/bililive-go/bililive-go/src/live"
	"github.com/bililive-go/bililive-go/src/recorders"
	log "github.com/sirupsen/logrus"
)

const (
	defaultInterval = 10 * time.Second
	minInterval     = 3 * time.Second
	maxInterval     = time.Hour
	maxSamples      = 5000
	requestTimeout  = 10 * time.Second

	// 房间观众数只能从直播页的服务端渲染数据里取，更新频率低于人气接口。
	audienceInterval = 15 * time.Second

	userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/156.0.0.0 Safari/537.36"
	spiURL  = "https://api.bilibili.com/x/frontend/finger/spi"
	h5URL   = "https://api.live.bilibili.com/xlive/web-room/v1/index/getH5InfoByRoom?room_id=%s"
	roomURL = "https://live.bilibili.com/%s"
	neptune = "window.__NEPTUNE_IS_MY_WAIFU__="
	csvHead = "ts,ts_unix,offset_s,room_id,host,title,live_status,popularity,watched,likes,recording,audience\n"
)

var (
	roomIDPattern = regexp.MustCompile(`(?i)live\.bilibili\.com/(\d+)`)

	defaultMu  sync.RWMutex
	defaultMgr *Manager
)

// Sample 是一次人气采样。
type Sample struct {
	TS         string `json:"ts"`
	TSUnix     int64  `json:"ts_unix"`
	OffsetSec  *int64 `json:"offset_s,omitempty"`
	RoomID     string `json:"room_id"`
	Host       string `json:"host"`
	Title      string `json:"title"`
	LiveStatus int    `json:"live_status"`
	Popularity int64  `json:"popularity"`
	Watched    int64  `json:"watched"`
	Likes      int64  `json:"likes"`
	Audience   *int64 `json:"audience,omitempty"`
	Recording  bool   `json:"recording"`
}

// Status 描述采样器的运行状态。
type Status struct {
	Running      bool   `json:"running"`
	Output       string `json:"output"`
	IntervalSec  int    `json:"interval_sec"`
	LastSampleAt string `json:"last_sample_at"`
	LastError    string `json:"last_error"`
	RoomCount    int    `json:"room_count"`
	SampleCount  int    `json:"sample_count"`
}

// Manager 周期性从 B 站公开接口采样直播间人气，并把结果写入 CSV。
type Manager struct {
	mu           sync.RWMutex
	samples      []Sample
	lastSampleAt time.Time
	lastErr      string
	roomCount    int
	running      bool

	interval time.Duration
	output   string
	client   *biliClient

	audienceMu sync.Mutex
	audience   map[string]audienceEntry

	file   *os.File
	writer *csv.Writer
}

type audienceEntry struct {
	count int64
	at    time.Time
}

// Default 返回当前运行的采样器，未启动时为 nil。
func Default() *Manager {
	defaultMu.RLock()
	defer defaultMu.RUnlock()
	return defaultMgr
}

// Start 启动采样器。outPutPath 是录制输出目录，数据会写到它同级的 data/ 下。
func Start(ctx context.Context, inst *instance.Instance, outPutPath string) *Manager {
	interval := defaultInterval
	if v := os.Getenv("BILILIVE_VIEWER_INTERVAL"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			d := time.Duration(n) * time.Second
			if d >= minInterval && d <= maxInterval {
				interval = d
			}
		}
	}

	if outPutPath == "" {
		outPutPath = "."
	}
	output := filepath.Join(filepath.Dir(outPutPath), "data", "viewer_log.csv")

	m := &Manager{
		interval: interval,
		output:   output,
		client:   newBiliClient(),
		audience: make(map[string]audienceEntry),
	}

	if err := m.openOutput(); err != nil {
		m.setError(err)
		log.WithError(err).Warn("人气监控：初始化数据文件失败，采样结果将只保留在内存中")
	}

	defaultMu.Lock()
	defaultMgr = m
	defaultMu.Unlock()

	m.mu.Lock()
	m.running = true
	m.mu.Unlock()

	log.WithFields(log.Fields{
		"interval": interval.String(),
		"output":   output,
	}).Info("人气监控已启动")

	go m.run(ctx, inst)
	return m
}

func (m *Manager) openOutput() error {
	dir := filepath.Dir(m.output)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	fresh := true
	if st, err := os.Stat(m.output); err == nil && st.Size() > 0 {
		fresh = false
	}
	f, err := os.OpenFile(m.output, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	m.file = f
	m.writer = csv.NewWriter(f)
	if fresh {
		if _, err := f.WriteString("\xEF\xBB\xBF" + csvHead); err != nil {
			return err
		}
		m.writer.Flush()
	}
	return nil
}

func (m *Manager) closeOutput() {
	if m.writer != nil {
		m.writer.Flush()
	}
	if m.file != nil {
		_ = m.file.Close()
		m.file = nil
	}
}

func (m *Manager) run(ctx context.Context, inst *instance.Instance) {
	m.sampleOnce(ctx, inst)
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			m.mu.Lock()
			m.running = false
			m.mu.Unlock()
			m.closeOutput()
			log.Info("人气监控已停止")
			return
		case <-ticker.C:
			m.sampleOnce(ctx, inst)
		}
	}
}

func (m *Manager) sampleOnce(ctx context.Context, inst *instance.Instance) {
	if inst == nil {
		return
	}
	rooms := 0
	for _, l := range inst.Lives.Snapshot() {
		roomID := parseRoomID(l.GetRawUrl())
		if roomID == "" {
			continue
		}
		rooms++

		info := cachedInfo(inst, l)
		host := ""
		title := ""
		if info != nil {
			host = info.HostName
			title = info.RoomName
		}
		recording := recordingState(ctx, inst, l)

		data, err := m.client.roomInfo(ctx, roomID)
		if err != nil {
			m.setError(err)
			log.WithError(err).WithField("room_id", roomID).Debug("人气采样失败")
			continue
		}
		if data.Title != "" {
			title = data.Title
		}

		now := time.Now()
		s := Sample{
			TS:         now.Format("2006-01-02 15:04:05"),
			TSUnix:     now.Unix(),
			RoomID:     roomID,
			Host:       host,
			Title:      title,
			LiveStatus: data.LiveStatus,
			Popularity: data.Popularity,
			Watched:    data.Watched,
			Likes:      data.Likes,
			Recording:  recording,
		}
		if start := l.GetLastStartTime(); !start.IsZero() {
			offset := int64(now.Sub(start).Seconds())
			s.OffsetSec = &offset
		}
		s.Audience = m.audienceFor(ctx, roomID)

		m.appendSample(s)
	}

	m.mu.Lock()
	m.roomCount = rooms
	m.mu.Unlock()
}

func cachedInfo(inst *instance.Instance, l live.Live) *live.Info {
	if inst.Cache == nil {
		return nil
	}
	obj, err := inst.Cache.Get(l)
	if err != nil || obj == nil {
		return nil
	}
	info, _ := obj.(*live.Info)
	return info
}

// recordingState 与 /api/lives 的判断保持一致：以录制器是否真正在写为准。
func recordingState(ctx context.Context, inst *instance.Instance, l live.Live) bool {
	if inst.RecorderManager == nil {
		return false
	}
	mgr, ok := inst.RecorderManager.(recorders.Manager)
	if !ok {
		return false
	}
	if !mgr.HasRecorder(ctx, l.GetLiveId()) {
		return false
	}
	recorder, err := mgr.GetRecorder(ctx, l.GetLiveId())
	if err != nil {
		return false
	}
	return recorder.IsRecording()
}

func (m *Manager) appendSample(s Sample) {
	m.mu.Lock()
	m.samples = append(m.samples, s)
	if len(m.samples) > maxSamples {
		m.samples = m.samples[len(m.samples)-maxSamples:]
	}
	m.lastSampleAt = time.Now()
	m.lastErr = ""
	m.mu.Unlock()

	m.writeRow(s)
}

func (m *Manager) writeRow(s Sample) {
	if m.writer == nil {
		return
	}
	offset := ""
	if s.OffsetSec != nil {
		offset = strconv.FormatInt(*s.OffsetSec, 10)
	}
	recording := "0"
	if s.Recording {
		recording = "1"
	}
	audience := ""
	if s.Audience != nil {
		audience = strconv.FormatInt(*s.Audience, 10)
	}
	_ = m.writer.Write([]string{
		s.TS,
		strconv.FormatInt(s.TSUnix, 10),
		offset,
		s.RoomID,
		s.Host,
		s.Title,
		strconv.Itoa(s.LiveStatus),
		strconv.FormatInt(s.Popularity, 10),
		strconv.FormatInt(s.Watched, 10),
		strconv.FormatInt(s.Likes, 10),
		recording,
		audience,
	})
	m.writer.Flush()
}

func (m *Manager) setError(err error) {
	if err == nil {
		return
	}
	m.mu.Lock()
	m.lastErr = err.Error()
	m.mu.Unlock()
}

// Samples 返回最近的采样，limit <= 0 时返回全部（最多 maxSamples 条）。
func (m *Manager) Samples(limit int) []Sample {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if limit <= 0 || limit > len(m.samples) {
		limit = len(m.samples)
	}
	out := make([]Sample, limit)
	copy(out, m.samples[len(m.samples)-limit:])
	return out
}

// Snapshot 返回状态信息。
func (m *Manager) Snapshot() Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	last := ""
	if !m.lastSampleAt.IsZero() {
		last = m.lastSampleAt.Format("2006-01-02 15:04:05")
	}
	return Status{
		Running:      m.running,
		Output:       m.output,
		IntervalSec:  int(m.interval.Seconds()),
		LastSampleAt: last,
		LastError:    m.lastErr,
		RoomCount:    m.roomCount,
		SampleCount:  len(m.samples),
	}
}

func parseRoomID(rawURL string) string {
	if m := roomIDPattern.FindStringSubmatch(rawURL); m != nil {
		return m[1]
	}
	return ""
}

// audienceFor 返回房间观众数。页面渲染数据更新不快，按 audienceInterval 缓存。
func (m *Manager) audienceFor(ctx context.Context, roomID string) *int64 {
	m.audienceMu.Lock()
	entry, ok := m.audience[roomID]
	m.audienceMu.Unlock()
	if ok && time.Since(entry.at) < audienceInterval {
		v := entry.count
		return &v
	}

	count, err := m.client.audienceCount(ctx, roomID)
	if err != nil {
		log.WithError(err).WithField("room_id", roomID).Debug("房间观众获取失败")
		if ok {
			v := entry.count
			return &v
		}
		return nil
	}

	m.audienceMu.Lock()
	m.audience[roomID] = audienceEntry{count: count, at: time.Now()}
	m.audienceMu.Unlock()
	return &count
}

// audienceCount 从直播页服务端渲染的数据里取房间观众数。
func (c *biliClient) audienceCount(ctx context.Context, roomID string) (int64, error) {
	reqCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	body, err := c.get(reqCtx, fmt.Sprintf(roomURL, roomID), "https://live.bilibili.com/")
	if err != nil {
		return 0, err
	}
	raw, err := extractJSONObject(body, neptune)
	if err != nil {
		return 0, err
	}

	var page struct {
		RoomInfoRes struct {
			Data struct {
				RoomRankInfo struct {
					UserRankEntry struct {
						Contribution struct {
							Count *int64 `json:"count"`
						} `json:"user_contribution_rank_entry"`
					} `json:"user_rank_entry"`
				} `json:"room_rank_info"`
			} `json:"data"`
		} `json:"roomInfoRes"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return 0, err
	}
	count := page.RoomInfoRes.Data.RoomRankInfo.UserRankEntry.Contribution.Count
	if count == nil {
		return 0, fmt.Errorf("页面数据里没有房间观众数")
	}
	return *count, nil
}

// extractJSONObject 从页面里截取 marker 之后的第一个 JSON 对象。
func extractJSONObject(page []byte, marker string) ([]byte, error) {
	i := bytes.Index(page, []byte(marker))
	if i < 0 {
		return nil, fmt.Errorf("页面里没有 %s", marker)
	}
	start := bytes.IndexByte(page[i:], '{')
	if start < 0 {
		return nil, fmt.Errorf("页面数据格式异常")
	}
	start += i

	depth := 0
	inString := false
	escaped := false
	for k := start; k < len(page); k++ {
		ch := page[k]
		if inString {
			switch {
			case escaped:
				escaped = false
			case ch == '\\':
				escaped = true
			case ch == '"':
				inString = false
			}
			continue
		}
		switch ch {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return page[start : k+1], nil
			}
		}
	}
	return nil, fmt.Errorf("页面数据未闭合")
}

type biliClient struct {
	http   *http.Client
	mu     sync.Mutex
	cookie string
}

func newBiliClient() *biliClient {
	return &biliClient{http: &http.Client{Timeout: requestTimeout}}
}

func (c *biliClient) ensureBuvid() {
	c.mu.Lock()
	has := c.cookie != ""
	c.mu.Unlock()
	if !has {
		c.refreshBuvid()
	}
}

func (c *biliClient) refreshBuvid() {
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	body, err := c.get(ctx, spiURL, "https://live.bilibili.com/")
	if err != nil {
		log.WithError(err).Warn("人气监控：获取 buvid 失败")
		return
	}
	var resp struct {
		Code int `json:"code"`
		Data struct {
			B3 string `json:"b_3"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		log.WithError(err).Warn("人气监控：解析 buvid 响应失败")
		return
	}
	if resp.Data.B3 != "" {
		c.mu.Lock()
		c.cookie = "buvid3=" + resp.Data.B3
		c.mu.Unlock()
	}
}

type roomData struct {
	Title      string
	LiveStatus int
	Popularity int64
	Watched    int64
	Likes      int64
}

func (c *biliClient) roomInfo(ctx context.Context, roomID string) (*roomData, error) {
	c.ensureBuvid()
	referer := fmt.Sprintf("https://live.bilibili.com/%s", roomID)
	for attempt := 0; attempt < 2; attempt++ {
		reqCtx, cancel := context.WithTimeout(ctx, requestTimeout)
		body, err := c.get(reqCtx, fmt.Sprintf(h5URL, roomID), referer)
		cancel()
		if err != nil {
			return nil, err
		}

		var resp struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Data    struct {
				RoomInfo struct {
					Online     int64  `json:"online"`
					LiveStatus int    `json:"live_status"`
					Title      string `json:"title"`
				} `json:"room_info"`
				WatchedShow struct {
					Num int64 `json:"num"`
				} `json:"watched_show"`
				LikeInfoV3 struct {
					TotalLikes int64 `json:"total_likes"`
				} `json:"like_info_v3"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, fmt.Errorf("解析人气响应失败: %w", err)
		}
		if resp.Code == 0 {
			return &roomData{
				Title:      resp.Data.RoomInfo.Title,
				LiveStatus: resp.Data.RoomInfo.LiveStatus,
				Popularity: resp.Data.RoomInfo.Online,
				Watched:    resp.Data.WatchedShow.Num,
				Likes:      resp.Data.LikeInfoV3.TotalLikes,
			}, nil
		}
		if resp.Code == -352 && attempt == 0 {
			c.refreshBuvid()
			continue
		}
		return nil, fmt.Errorf("B 站接口返回 code=%d %s", resp.Code, resp.Message)
	}
	return nil, fmt.Errorf("B 站接口持续返回 -352（风控）")
}

func (c *biliClient) get(ctx context.Context, url, referer string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Referer", referer)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	c.mu.Lock()
	cookie := c.cookie
	c.mu.Unlock()
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}
