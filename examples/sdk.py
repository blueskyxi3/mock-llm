#!/usr/bin/env python3
"""Optional SDK verification: install openai in a separate virtual environment."""
import json
import os
from openai import OpenAI

client = OpenAI(base_url=os.environ.get('MOCK_BASE_URL', 'http://127.0.0.1:8080/v1'), api_key=os.environ.get('MOCK_API_KEY', 'local-test'))
response = client.chat.completions.create(model='mock-chat-short', messages=[{'role': 'user', 'content': '你好'}])
assert response.usage.total_tokens == response.usage.prompt_tokens + response.usage.completion_tokens
assert response.choices[0].message.content
chunks = list(client.chat.completions.create(model='mock-chat-short', messages=[{'role': 'user', 'content': 'hello'}], stream=True, stream_options={'include_usage': True}))
assert chunks[-1].usage is not None
assert any(c.choices and c.choices[0].delta.content for c in chunks)
tools = [{'type': 'function', 'function': {'name': 'calculate', 'parameters': {'type': 'object', 'properties': {'expression': {'type': 'string'}}, 'required': ['expression']}}}]
messages = [{'role': 'user', 'content': '计算 2+3'}]
first = client.chat.completions.create(model='mock-agent', messages=messages, tools=tools, tool_choice='required')
assistant = first.choices[0].message
assert json.loads(assistant.tool_calls[0].function.arguments)['expression'] == '2+3'
messages.append(assistant.model_dump(exclude_none=True))
messages.append({'role': 'tool', 'tool_call_id': assistant.tool_calls[0].id, 'content': '{"value":5}'})
final = client.chat.completions.create(model='mock-agent', messages=messages, tools=tools)
assert final.choices[0].finish_reason == 'stop'
embedding = client.embeddings.create(model='mock-embedding', input=['你好', 'hello'], dimensions=64, encoding_format='float')
assert len(embedding.data) == 2 and len(embedding.data[0].embedding) == 64
print('SDK chat, SSE, agent round-trip and embeddings verified')
