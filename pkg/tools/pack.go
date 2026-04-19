package tools

import (
	"google.golang.org/adk/model"
	"google.golang.org/adk/tool"
	"google.golang.org/genai"
)

// declarationer mirrors ADK's unexported "runnableTool" — implemented by
// both our system tools and mcptoolset tools.
type declarationer interface {
	Declaration() *genai.FunctionDeclaration
}

// Pack adds a single Tool to the LLM request: registers it in req.Tools
// (dispatch map by name) and appends its function declaration into
// req.Config.Tools (LLM-visible schema). Idempotent per name.
//
// Mirrors the internal toolutils.PackTool from ADK (unimportable), so our
// BeforeModelCallback can bypass ADK's Flow-level Tools cache.
func Pack(req *model.LLMRequest, t tool.Tool) {
	name := t.Name()
	if req.Tools == nil {
		req.Tools = make(map[string]any)
	}
	if _, ok := req.Tools[name]; ok {
		return
	}
	req.Tools[name] = t

	d, ok := t.(declarationer)
	if !ok {
		return
	}
	decl := d.Declaration()
	if decl == nil {
		return
	}
	if req.Config == nil {
		req.Config = &genai.GenerateContentConfig{}
	}
	var funcTool *genai.Tool
	for _, ft := range req.Config.Tools {
		if ft != nil && ft.FunctionDeclarations != nil {
			funcTool = ft
			break
		}
	}
	if funcTool == nil {
		req.Config.Tools = append(req.Config.Tools, &genai.Tool{
			FunctionDeclarations: []*genai.FunctionDeclaration{decl},
		})
	} else {
		funcTool.FunctionDeclarations = append(funcTool.FunctionDeclarations, decl)
	}
}
