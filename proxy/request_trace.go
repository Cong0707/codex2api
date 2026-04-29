package proxy

import (
	"log"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func requestTraceID(c *gin.Context) string {
	if c != nil {
		if v, ok := c.Get("x-request-id"); ok {
			if id, ok := v.(string); ok && id != "" {
				return id
			}
		}
	}
	id := uuid.NewString()
	if len(id) > 8 {
		id = id[:8]
	}
	if c != nil {
		c.Set("x-request-id", id)
	}
	return id
}

func elapsedMsBetween(start, end time.Time) int {
	if start.IsZero() || end.IsZero() || end.Before(start) {
		return 0
	}
	return int(end.Sub(start).Milliseconds())
}

func accountTraceFields(acc *auth.Account) (id int64, email string, plan string) {
	if acc == nil {
		return 0, "", ""
	}
	id = acc.ID()
	plan = acc.GetPlanType()
	acc.Mu().RLock()
	email = acc.Email
	acc.Mu().RUnlock()
	return id, security.MaskEmail(email), security.SanitizeLog(plan)
}

func logRequestLifecycleStart(c *gin.Context, endpoint, model string, stream bool, effort string) {
	rid := requestTraceID(c)
	extra := ""
	if effort = security.SanitizeLog(effort); effort != "" {
		extra = " effort=" + effort
	}
	log.Printf("请求开始: rid=%s endpoint=%s model=%s stream=%t%s", rid, security.SanitizeLog(endpoint), security.SanitizeLog(model), stream, extra)
}

func logRequestLifecycleStartOnce(c *gin.Context, endpoint, model string, stream bool, effort string) {
	if c != nil {
		if logged, ok := c.Get("x-request-lifecycle-logged"); ok {
			if done, ok := logged.(bool); ok && done {
				return
			}
		}
		c.Set("x-request-lifecycle-logged", true)
	}
	logRequestLifecycleStart(c, endpoint, model, stream, effort)
}

func logRequestDispatch(c *gin.Context, endpoint string, attempt int, acc *auth.Account, proxyURL, model, effort string, attemptAcquireMs int) {
	rid := requestTraceID(c)
	accountID, email, plan := accountTraceFields(acc)
	schedMs := int64(0)
	if v, ok := c.Get("x-scheduler-acquire-ms"); ok {
		if ms, ok := v.(int64); ok {
			schedMs = ms
		}
	}
	waitRounds := 0
	if v, ok := c.Get("x-scheduler-wait-rounds"); ok {
		if n, ok := v.(int); ok {
			waitRounds = n
		}
	}
	if c != nil && attemptAcquireMs > 0 {
		c.Set("x-scheduler-attempt-ms", attemptAcquireMs)
	}
	extra := ""
	if effort = security.SanitizeLog(effort); effort != "" {
		extra = " effort=" + effort
	}
	log.Printf("请求调度完成: rid=%s endpoint=%s attempt=%d sched=%dms pick=%dms wait_rounds=%d account=%d email=%s plan=%s model=%s%s proxy=%s",
		rid,
		security.SanitizeLog(endpoint),
		attempt,
		schedMs,
		attemptAcquireMs,
		waitRounds,
		accountID,
		email,
		plan,
		security.SanitizeLog(model),
		extra,
		security.SanitizeLog(proxyURL),
	)
}

func logUpstreamAttemptHeaders(c *gin.Context, endpoint string, attempt int, acc *auth.Account, statusCode int, trace *UpstreamAttemptTrace, requestStartedAt time.Time) {
	if trace == nil {
		return
	}
	rid := requestTraceID(c)
	accountID, email, plan := accountTraceFields(acc)
	requestHeaderMs := elapsedMsBetween(requestStartedAt, trace.HeaderAt)
	attemptHeaderMs := trace.HeaderMs()
	if requestHeaderMs > 0 {
		c.Set("x-upstream-header-ms", requestHeaderMs)
	}
	if firstByteMs := trace.FirstResponseByteMs(); firstByteMs > 0 {
		c.Set("x-upstream-first-byte-ms", firstByteMs)
	}
	if connectMs := trace.ConnectMs(); connectMs > 0 {
		c.Set("x-upstream-connect-ms", connectMs)
	}
	if dnsMs := trace.DNSMs(); dnsMs > 0 {
		c.Set("x-upstream-dns-ms", dnsMs)
	}
	if tlsMs := trace.TLSMs(); tlsMs > 0 {
		c.Set("x-upstream-tls-ms", tlsMs)
	}
	c.Set("x-upstream-reused-conn", trace.ReusedConn)
	log.Printf("请求上游响应头: rid=%s endpoint=%s attempt=%d account=%d email=%s plan=%s status=%d request_header=%dms header=%dms connect=%dms dns=%dms tls=%dms first_byte=%dms reused=%t idle=%t transport=%s proxy=%s",
		rid,
		security.SanitizeLog(endpoint),
		attempt,
		accountID,
		email,
		plan,
		statusCode,
		requestHeaderMs,
		attemptHeaderMs,
		trace.ConnectMs(),
		trace.DNSMs(),
		trace.TLSMs(),
		trace.FirstResponseByteMs(),
		trace.ReusedConn,
		trace.WasIdleConn,
		security.SanitizeLog(trace.Transport),
		security.SanitizeLog(trace.ProxyURL),
	)
}

func logUpstreamFirstFrame(c *gin.Context, endpoint string, attempt int, eventType string, requestStartedAt, attemptStartedAt time.Time) {
	rid := requestTraceID(c)
	requestFrameMs := elapsedMsBetween(requestStartedAt, time.Now())
	attemptFrameMs := elapsedMsBetween(attemptStartedAt, time.Now())
	if requestFrameMs > 0 {
		c.Set("x-upstream-first-frame-ms", requestFrameMs)
	}
	log.Printf("请求上游首帧: rid=%s endpoint=%s attempt=%d request_frame=%dms frame=%dms type=%s",
		rid,
		security.SanitizeLog(endpoint),
		attempt,
		requestFrameMs,
		attemptFrameMs,
		security.SanitizeLog(eventType),
	)
}

func logUpstreamFirstVisible(c *gin.Context, endpoint string, attempt int, eventType string, requestStartedAt, attemptStartedAt time.Time) {
	rid := requestTraceID(c)
	requestVisibleMs := elapsedMsBetween(requestStartedAt, time.Now())
	attemptVisibleMs := elapsedMsBetween(attemptStartedAt, time.Now())
	if requestVisibleMs > 0 {
		c.Set("x-upstream-first-visible-ms", requestVisibleMs)
		c.Set("x-first-token-ms", requestVisibleMs)
	}
	log.Printf("请求上游首正文: rid=%s endpoint=%s attempt=%d request_visible=%dms visible=%dms type=%s",
		rid,
		security.SanitizeLog(endpoint),
		attempt,
		requestVisibleMs,
		attemptVisibleMs,
		security.SanitizeLog(eventType),
	)
}

func logUpstreamAttemptResult(c *gin.Context, endpoint string, attempt int, acc *auth.Account, proxyURL string, statusCode int, attemptTotalMs int, requestStartedAt time.Time, failure string) {
	rid := requestTraceID(c)
	accountID, email, plan := accountTraceFields(acc)
	requestTotalMs := elapsedMsBetween(requestStartedAt, time.Now())
	if attemptTotalMs > 0 {
		c.Set("x-upstream-attempt-total-ms", attemptTotalMs)
	}
	if requestTotalMs > 0 {
		c.Set("x-request-total-ms", requestTotalMs)
	}
	failure = security.SanitizeLog(failure)
	if failure != "" {
		log.Printf("请求尝试结束: rid=%s endpoint=%s attempt=%d account=%d email=%s plan=%s status=%d attempt_total=%dms request_total=%dms failure=%s proxy=%s",
			rid,
			security.SanitizeLog(endpoint),
			attempt,
			accountID,
			email,
			plan,
			statusCode,
			attemptTotalMs,
			requestTotalMs,
			failure,
			security.SanitizeLog(proxyURL),
		)
		return
	}
	log.Printf("请求尝试结束: rid=%s endpoint=%s attempt=%d account=%d email=%s plan=%s status=%d attempt_total=%dms request_total=%dms proxy=%s",
		rid,
		security.SanitizeLog(endpoint),
		attempt,
		accountID,
		email,
		plan,
		statusCode,
		attemptTotalMs,
		requestTotalMs,
		security.SanitizeLog(proxyURL),
	)
}
