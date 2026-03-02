## Agent API 接口文档
说明：选手agent程序是运行在本地的，并启动监听端口，启动后将监听端口配置到大赛平台Agent配置中去

### 基础信息
Base URL: http://localhost:8191
Content-Type: application/json
### 对话接口
#### POST /api/v1/chat
请求参数
|字段	|类型	|必填	|说明|
|---|---|---|---|
|model_ip	|string	|是	|模型资源接口IP，端口固定为8888|
|session_id	|string	|是	|会话ID，对于多轮的用例，会使用同一个 session_id 调用多次 Agent，所以同一个session_id 时需要做好上下文管理|
|message	|string	|是	|用户消息|

请求示例
```
curl -X POST http://localhost:8191/api/v1/chat \
  -H "Content-Type: application/json" \
  -d '{
    "model_ip":"xxx.xxx.xx.x",
    "session_id": "abc123",
    "message": "查询海淀区的房源"
  }'
```

响应参数
|字段	|类型	|说明|
|---|---|---|
|session_id	|string	|会话ID|
|response	|string	|Agent回复|
|status	|string	|处理状态|
|tool_results	|array	|工具调用结果|
|timestamp	|int	|时间戳|
|duration_ms	|int	|处理耗时(毫秒)|

响应示例
```
{
  "session_id": "abc123",
  "response": "为您找到海淀区3套房源...",
  "status": "success",
  "tool_results": [
    {
      "name": "bash",
      "success": true,
      "output": "..."
    }
  ],
  "timestamp": 1704067200,
  "duration_ms": 1500
}
```
