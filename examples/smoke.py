#!/usr/bin/env python3
"""Local mock or AI Hub channel health check; Python standard library only."""
import argparse
import json
import math
import os
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path


def require(condition, message):
    if not condition:
        raise ValueError(message)


def check_usage(usage):
    require(isinstance(usage, dict), 'missing usage')
    values = [usage.get(name) for name in ('prompt_tokens', 'completion_tokens', 'total_tokens')]
    require(all(type(value) is int and value >= 0 for value in values), 'invalid usage')
    require(values[0] + values[1] == values[2], 'usage total mismatch')


def check_stream(response, model):
    identity = None
    text, finished, done, usage = [], False, False, None
    for raw in response:
        line = raw.decode('utf-8').strip()
        if not line.startswith('data: '):
            continue
        data = line[6:]
        if data == '[DONE]':
            done = True
            break
        chunk = json.loads(data)
        require('error' not in chunk, 'SSE returned an error')
        require(chunk.get('model') == model, 'unexpected SSE model')
        require(chunk.get('object') == 'chat.completion.chunk', 'invalid SSE object')
        require(bool(chunk.get('id')), 'missing SSE response id')
        if identity is None:
            identity = chunk['id']
        require(identity == chunk['id'], 'SSE response id changed')
        if chunk.get('usage') is not None:
            usage = chunk['usage']
        for choice in chunk.get('choices', []):
            text.append(choice.get('delta', {}).get('content') or '')
            if choice.get('finish_reason') is not None:
                require(choice['finish_reason'] in ('stop', 'length'), 'unexpected SSE finish reason')
                finished = True
    require(done and finished and bool(''.join(text)), 'incomplete SSE response or missing [DONE]')
    check_usage(usage)
    return {'response_id': identity, 'usage': usage}


def run(args):
    base = args.base_url.rstrip('/')
    parsed = urllib.parse.urlsplit(base)
    require(parsed.scheme in ('http', 'https') and bool(parsed.netloc), 'invalid base URL')
    require(not parsed.username and not parsed.password and not parsed.query and not parsed.fragment,
            'base URL must not contain credentials, query or fragment')
    require(base.endswith('/v1'), 'base URL must end with /v1')
    require(args.timeout > 0, 'timeout must be positive')
    key = os.environ.get(args.api_key_env, '')
    if args.mode == 'gateway':
        require(bool(key), 'gateway API key environment variable is empty')
    if args.channel_id is not None:
        require(args.mode == 'gateway' and args.channel_id > 0, 'channel-id requires gateway mode and a positive ID')
        # AI Hub supports administrator relay tokens suffixed with a channel ID.
        key = key + '#' + str(args.channel_id)
    headers = {'Content-Type': 'application/json'}
    if key:
        headers['Authorization'] = 'Bearer ' + key

    def request(path, payload=None, root=False):
        url = (base[:-3] if root else base) + path
        body = None if payload is None else json.dumps(payload, ensure_ascii=False).encode('utf-8')
        return urllib.request.urlopen(urllib.request.Request(url, data=body, headers=headers), timeout=args.timeout)

    def call(path, payload=None, root=False):
        with request(path, payload, root) as response:
            return json.load(response)

    results = []

    def check(name, action):
        start = time.monotonic()
        item = {'name': name}
        try:
            item['details'] = action()
            item['ok'] = True
        except urllib.error.HTTPError as error:
            # Never print credentials, request bodies, or untrusted upstream error bodies.
            item.update(ok=False, error='HTTP ' + str(error.code))
        except (OSError, ValueError, KeyError, IndexError, TypeError) as error:
            item.update(ok=False, error=type(error).__name__ + ': ' + str(error))
        item['seconds'] = round(time.monotonic() - start, 3)
        results.append(item)

    def readiness():
        if args.mode == 'direct':
            require(call('/healthz', root=True).get('status') == 'ok', 'healthz failed')
            require(call('/readyz', root=True).get('ready') is True, 'service is not ready')
        else:
            require(call('/api/status', root=True).get('success') is True, 'gateway status failed')
        return {'ready': True}

    def models():
        body = call('/models')
        names = {item['id'] for item in body['data']}
        require(args.model in names and args.embedding_model in names, 'required mock models are not listed')
        return {'required_models_available': True}

    payload = {'model': args.model, 'max_completion_tokens': 32,
               'messages': [{'role': 'user', 'content': '健康检查：hello，你好。'}]}

    def chat():
        body = call('/chat/completions', payload)
        require(body.get('model') == args.model and body.get('object') == 'chat.completion', 'invalid chat response')
        choice = body['choices'][0]
        require(bool(choice['message']['content']), 'empty chat content')
        require(choice['finish_reason'] in ('stop', 'length'), 'unexpected chat finish reason')
        check_usage(body.get('usage'))
        return {'response_id': body['id'], 'usage': body['usage']}

    def stream():
        with request('/chat/completions', dict(payload, stream=True, stream_options={'include_usage': True})) as response:
            require('text/event-stream' in response.headers.get('Content-Type', ''), 'invalid SSE content type')
            return check_stream(response, args.model)

    def embeddings():
        body = call('/embeddings', {'model': args.embedding_model, 'input': ['你好', 'hello'],
                                    'dimensions': 64, 'encoding_format': 'float'})
        require(body.get('model') == args.embedding_model and len(body['data']) == 2, 'invalid embedding batch')
        for index, item in enumerate(body['data']):
            vector = item['embedding']
            require(item['index'] == index and len(vector) == 64, 'invalid embedding index or dimension')
            require(all(isinstance(value, (int, float)) and math.isfinite(value) for value in vector), 'invalid vector values')
            require(abs(sum(value * value for value in vector) - 1) < 1e-5, 'vector is not normalized')
        usage = body['usage']
        require(type(usage.get('prompt_tokens')) is int and usage['prompt_tokens'] > 0 and
                usage.get('total_tokens') == usage['prompt_tokens'], 'invalid embedding usage')
        return {'batch': 2, 'dimensions': 64}

    check('readiness', readiness)
    check('models', models)
    check('chat', chat)
    check('sse', stream)
    check('embeddings', embeddings)
    return {'ok': all(item['ok'] for item in results), 'mode': args.mode,
            'base_url': base, 'channel_id': args.channel_id, 'checks': results,
            'scope': 'HTTP and protocol health; does not confirm billing settlement or maximum capacity'}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--base-url', default=os.environ.get('MOCK_BASE_URL', 'http://127.0.0.1:18080/v1'))
    parser.add_argument('--mode', choices=('direct', 'gateway'), default='direct')
    parser.add_argument('--api-key-env', default='MOCK_API_KEY')
    parser.add_argument('--channel-id', type=int, help='force an AI Hub channel using an administrator relay token')
    parser.add_argument('--model', default='mock-chat-short')
    parser.add_argument('--embedding-model', default='mock-embedding')
    parser.add_argument('--timeout', type=float, default=15)
    parser.add_argument('--output', type=Path)
    args = parser.parse_args()
    try:
        result = run(args)
    except ValueError as error:
        result = {'ok': False, 'error': str(error)}
    text = json.dumps(result, ensure_ascii=False, indent=2)
    if args.output:
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_text(text + '\n', encoding='utf-8')
    print(text)
    return 0 if result['ok'] else 1


if __name__ == '__main__':
    sys.exit(main())
