package envprobe

import (
	"context"
	"errors"

	"github.com/agenthub/agent-proxy/internal/tool"
)

// ToolName is the registry name of the environment probe.
//
// It is the step the agent takes before it promises anything: the user says
// what they want, and this is how the agent finds out whether this machine can
// do it, needs something installed, or needs a while to be worked out
// (DESIGN §9.3).
const ToolName = "env_probe"

// RegisterTools mounts the probe. The probe is read-only and takes no
// configuration, so unlike the control surface there is no module switch to
// consult — it reads directory names and PATH, and nothing here can change the
// machine.
func RegisterTools(reg interface{ Register(tool.Tool) }) {
	reg.Register(probeTool{})
}

type probeTool struct{}

func (probeTool) Name() string { return ToolName }

func (probeTool) Description() string {
	return "看看这台电脑上有什么能用来做这件事：给一句话描述（例如「我要做一个 2.5D 游戏」），" +
		"返回三组结果——可以直接开工的、装了但只能操作界面（需要先花点时间摸清怎么驱动）的、本机没有的。" +
		"用户说想做什么之后先调它，再决定是直接干、让用户等一下、还是提示安装。只读，不改动电脑。"
}

func (probeTool) Schema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"query": map[string]interface{}{
				"type":        "string",
				"description": "用户想做的事，用他们自己的话说，例如「做一个 2.5D 游戏」「剪一段视频」",
			},
		},
		"required": []string{"query"},
	}
}

func (probeTool) Execute(_ context.Context, args map[string]interface{}) (string, error) {
	q, _ := args["query"].(string)
	if q == "" {
		return "", errors.New("缺少 query：请把用户想做的事填进来")
	}
	return Render(Probe(q)), nil
}
