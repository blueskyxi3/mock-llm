#!/usr/bin/env python3
"""Standard-library examples; works directly or through an AI Hub channel."""
import argparse
import json
import math
import os
import time
import urllib.request

BASE = os.environ.get('MOCK_BASE_URL', 'http://127.0.0.1:8080/v1').rstrip('/')
KEY = os.environ.get('MOCK_API_KEY', 'local-test')
DOCS = [
    {'id': 'doc-1', 'text': 'AI Hub 在调用供应商前预扣，请求完成后发送账务事件，由 Worker 结算。'},
    {'id': 'doc-2', 'text': 'SQL 与 Redis 通过幂等差额和持久凭证补偿保持最终一致。'},
]
TOOLS = [
    {'type': 'function', 'function': {'name': 'search_docs', 'description': '查询本地示例文档', 'parameters': {'type': 'object', 'properties': {'query': {'type': 'string'}}, 'required': ['query']}}},
    {'type': 'function', 'function': {'name': 'calculate', 'description': '本地示例计算', 'parameters': {'type': 'object', 'properties': {'expression': {'type': 'string'}}, 'required': ['expression']}}},
]

def post(path, payload):
    request = urllib.request.Request(BASE + path, data=json.dumps(payload, ensure_ascii=False).encode(), headers={'Authorization': 'Bearer ' + KEY, 'Content-Type': 'application/json'})
    return urllib.request.urlopen(request, timeout=125)

def call(path, payload):
    with post(path, payload) as response:
        return json.load(response)

def chat():
    payload = {'model': 'mock-chat-short', 'messages': [{'role': 'system', 'content': '请简洁回答。'}, {'role': 'user', 'content': '你好，请介绍 AI 网关。'}]}
    print(json.dumps(call('/chat/completions', payload), ensure_ascii=False, indent=2))

def stream():
    payload = {'model': 'mock-stream-30s', 'stream': True, 'stream_options': {'include_usage': True}, 'messages': [{'role': 'user', 'content': '请给出较长的中文回答。'}]}
    start = time.monotonic()
    with post('/chat/completions', payload) as response:
        for line in response:
            line = line.decode().strip()
            if line.startswith('data: '):
                print(f'{time.monotonic() - start:.3f}s {line}', flush=True)

def agent():
    messages = [{'role': 'user', 'content': '检索 AI Hub 账务资料，并计算 2+3。'}]
    first = call('/chat/completions', {'model': 'mock-agent', 'messages': messages, 'tools': TOOLS})
    assistant = first['choices'][0]['message']
    messages.append(assistant)
    for tool in assistant['tool_calls']:
        name = tool['function']['name']
        arguments = json.loads(tool['function']['arguments'])
        if name == 'search_docs':
            result = {'query': arguments['query'], 'documents': DOCS}
        elif name == 'calculate' and arguments['expression'] == '2+3':
            result = {'value': 5}
        else:
            raise ValueError('tool fixture is not supported; no arbitrary code is executed')
        messages.append({'role': 'tool', 'tool_call_id': tool['id'], 'content': json.dumps(result, ensure_ascii=False)})
    final = call('/chat/completions', {'model': 'mock-agent', 'messages': messages, 'tools': TOOLS})
    print(json.dumps({'tool_response': first, 'final_response': final}, ensure_ascii=False, indent=2))

def rag():
    question = 'AI Hub 如何保证 SQL 和 Redis 最终一致？'
    vectors = call('/embeddings', {'model': 'mock-embedding', 'input': [doc['text'] for doc in DOCS] + [question], 'dimensions': 64})['data']
    query = vectors[-1]['embedding']
    ranked = sorted(zip(DOCS, vectors[:-1]), key=lambda pair: sum(a*b for a,b in zip(query, pair[1]['embedding'])), reverse=True)
    # Both documents are retained: synthetic vectors do not encode semantics.
    context = '\n'.join(f"[{doc['id']}] {doc['text']}" for doc, _ in ranked)
    answer = call('/chat/completions', {'model': 'mock-rag', 'messages': [{'role': 'system', 'content': '根据文档回答并给出引用。'}, {'role': 'user', 'content': context + '\n问题：' + question}]})
    print(json.dumps({'retrieved': [doc['id'] for doc,_ in ranked], 'answer': answer, 'warning': '模拟向量只验证接口和流程，不评估语义质量'}, ensure_ascii=False, indent=2))

def long_input():
    context = ('这是一段用于长上下文测试的文档。The gateway settles billing after completion.\n' * 1000)
    answer = call('/chat/completions', {'model': 'mock-chat-long', 'max_completion_tokens': 1024, 'messages': [{'role': 'user', 'content': context + '请归纳内容。'}]})
    print(json.dumps(answer, ensure_ascii=False, indent=2))

if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('scenario', choices=['chat', 'stream', 'agent', 'rag', 'long'])
    opts = parser.parse_args()
    {'chat': chat, 'stream': stream, 'agent': agent, 'rag': rag, 'long': long_input}[opts.scenario]()
