package webimage

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

type ReferenceImage struct {
	Data     []byte
	FileName string
}

type GenerateRequest struct {
	Prompt          string
	ReferenceImages []ReferenceImage
	PollMaxWait     time.Duration
	UpstreamModel   string
	MaxBytes        int64
}

type GeneratedImage struct {
	Data        []byte
	ContentType string
	FileRef     string
	SignedURL   string
}

type GenerateResult struct {
	ConversationID string
	Images         []GeneratedImage
}

func (c *Client) Generate(ctx context.Context, req GenerateRequest) (*GenerateResult, error) {
	if c == nil {
		return nil, errors.New("client is nil")
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return nil, errors.New("prompt is required")
	}
	if req.PollMaxWait <= 0 {
		req.PollMaxWait = 180 * time.Second
	}
	if strings.TrimSpace(req.UpstreamModel) == "" {
		req.UpstreamModel = "auto"
	}
	if req.MaxBytes <= 0 {
		req.MaxBytes = 16 * 1024 * 1024
	}
	referenceHashes := referenceImageHashSet(req.ReferenceImages)

	cr, err := c.ChatRequirementsV2(ctx)
	if err != nil {
		return nil, err
	}

	var proofToken string
	if cr.Proofofwork.Required {
		proofToken = SolveProofToken(cr.Proofofwork.Seed, cr.Proofofwork.Difficulty, c.opts.UserAgent)
		if proofToken == "" {
			return nil, errors.New("solve proof token failed")
		}
	}

	var refs []*UploadedFile
	if len(req.ReferenceImages) > 0 {
		for idx, item := range req.ReferenceImages {
			up, upErr := c.UploadFile(ctx, item.Data, item.FileName)
			if upErr != nil {
				return nil, fmt.Errorf("upload reference %d failed: %w", idx, upErr)
			}
			refs = append(refs, up)
		}
	}

	convOpt := ImageConvOpts{
		Prompt:        req.Prompt,
		UpstreamModel: "auto",
		ParentMsgID:   uuid.NewString(),
		MessageID:     uuid.NewString(),
		ChatToken:     cr.Token,
		ProofToken:    proofToken,
		References:    refs,
	}
	if req.UpstreamModel != "" && req.UpstreamModel != "auto" && !cr.IsFreeAccount() {
		convOpt.UpstreamModel = req.UpstreamModel
	}

	if conduitToken, prepErr := c.PrepareFConversation(ctx, convOpt); prepErr == nil {
		convOpt.ConduitToken = conduitToken
	}

	stream, err := c.StreamFConversation(ctx, convOpt)
	if err != nil {
		return nil, err
	}
	parsed := ParseImageSSE(stream)
	convID := strings.TrimSpace(parsed.ConversationID)

	fileRefs := make([]string, 0, len(parsed.FileIDs)+len(parsed.SedimentIDs))
	fileRefs = append(fileRefs, parsed.FileIDs...)
	for _, sid := range parsed.SedimentIDs {
		fileRefs = append(fileRefs, "sed:"+sid)
	}

	if len(fileRefs) == 0 {
		if convID == "" {
			return nil, errors.New("image conversation id missing")
		}
		status, fids, sids := c.PollConversationForImages(ctx, convID, PollOpts{
			ExpectedN: 1,
			MaxWait:   req.PollMaxWait,
		})
		switch status {
		case PollStatusSuccess:
			fileRefs = append(fileRefs, fids...)
			for _, sid := range sids {
				fileRefs = append(fileRefs, "sed:"+sid)
			}
		case PollStatusTimeout:
			return nil, errors.New("poll image timeout")
		default:
			return nil, errors.New("poll image failed")
		}
	}

	if len(refs) > 0 {
		refSet := referenceUploadFileIDSet(refs)
		fileRefs = filterOutReferenceFileIDs(fileRefs, refSet)
		// 图生图/编辑时，SSE 经常会先回显用户上传的 file-service:// 参考图。
		// 如果过滤后没有真正生成图，不要直接失败，也不要把原图返回给下游；
		// 继续轮询 conversation 的 image_gen tool 消息补拿本轮生成结果。
		if len(fileRefs) == 0 && convID != "" {
			status, fids, sids := c.PollConversationForImages(ctx, convID, PollOpts{
				ExpectedN: 1,
				MaxWait:   req.PollMaxWait,
			})
			if status == PollStatusSuccess {
				fileRefs = append(fileRefs, fids...)
				for _, sid := range sids {
					fileRefs = append(fileRefs, "sed:"+sid)
				}
				fileRefs = filterOutReferenceFileIDs(dedupeFileRefs(fileRefs), refSet)
			}
		}
	}
	if len(fileRefs) == 0 {
		return nil, errors.New("no generated image reference returned")
	}

	result := &GenerateResult{ConversationID: convID}
	for _, ref := range fileRefs {
		signedURL, urlErr := c.ImageDownloadURL(ctx, convID, ref)
		if urlErr != nil {
			continue
		}
		data, contentType, fetchErr := c.FetchImage(ctx, signedURL, req.MaxBytes)
		if fetchErr != nil {
			continue
		}
		if isReferenceImageBytes(data, referenceHashes) {
			continue
		}
		result.Images = append(result.Images, GeneratedImage{
			Data:        data,
			ContentType: contentType,
			FileRef:     ref,
			SignedURL:   signedURL,
		})
		break
	}
	if len(result.Images) == 0 {
		if len(referenceHashes) > 0 {
			return nil, errors.New("generated image fetch returned only reference image")
		}
		return nil, errors.New("fetch generated image failed")
	}
	return result, nil
}

func referenceUploadFileIDSet(refs []*UploadedFile) map[string]struct{} {
	out := make(map[string]struct{})
	for _, item := range refs {
		if item == nil {
			continue
		}
		id := strings.TrimSpace(item.FileID)
		if id == "" {
			continue
		}
		out[strings.TrimPrefix(id, "sed:")] = struct{}{}
	}
	return out
}

func dedupeFileRefs(fileRefs []string) []string {
	if len(fileRefs) <= 1 {
		return fileRefs
	}
	seen := make(map[string]struct{}, len(fileRefs))
	out := make([]string, 0, len(fileRefs))
	for _, ref := range fileRefs {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			continue
		}
		if _, ok := seen[ref]; ok {
			continue
		}
		seen[ref] = struct{}{}
		out = append(out, ref)
	}
	return out
}

func referenceImageHashSet(refs []ReferenceImage) map[[32]byte]struct{} {
	if len(refs) == 0 {
		return nil
	}
	out := make(map[[32]byte]struct{}, len(refs))
	for _, ref := range refs {
		if len(ref.Data) == 0 {
			continue
		}
		out[sha256.Sum256(ref.Data)] = struct{}{}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func isReferenceImageBytes(data []byte, refHashes map[[32]byte]struct{}) bool {
	if len(data) == 0 || len(refHashes) == 0 {
		return false
	}
	_, ok := refHashes[sha256.Sum256(data)]
	return ok
}

func filterOutReferenceFileIDs(fileRefs []string, refSet map[string]struct{}) []string {
	if len(refSet) == 0 {
		return fileRefs
	}
	out := make([]string, 0, len(fileRefs))
	for _, ref := range fileRefs {
		if strings.HasPrefix(ref, "sed:") {
			out = append(out, ref)
			continue
		}
		if _, exists := refSet[ref]; exists {
			continue
		}
		out = append(out, ref)
	}
	return out
}
