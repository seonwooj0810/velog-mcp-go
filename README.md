# velog-mcp-go

[velog-mcp](https://github.com/seonwoo-jung/velog-mcp)의 Go 포팅. TypeScript 버전과 기능·옵션·환경변수가 1:1로 동일하며, 공식 [`modelcontextprotocol/go-sdk`](https://github.com/modelcontextprotocol/go-sdk)를 사용한다.

## 노출되는 도구

| 도구 | 인증 필요 | 설명 |
|------|-----------|------|
| `velog_whoami` | ✅ | 토큰 유효성·로그인 사용자 정보 확인 |
| `velog_list_posts` | ⛔ | 사용자/태그/커서로 글 목록 조회 (`temp_only`는 ✅) |
| `velog_get_post` | ⛔ | `id` 또는 `username + url_slug`로 단건 조회 |
| `velog_write_post` | ✅ | 새 글 작성 (`is_temp=true`면 임시저장) |
| `velog_edit_post` | ✅ | 기존 글 수정 (모든 필수 필드 재전송) |

## 빌드

```bash
cd /Users/seonwoo_jung/workspace/velog-mcp-go
go build -o velog-mcp-go
```

산출물: `./velog-mcp-go` (단일 실행 바이너리, stdio MCP 서버).

## 토큰 얻기

Velog는 공식 토큰 발급 API가 없으므로 브라우저 쿠키에서 가져온다.

1. Chrome/Safari에서 https://velog.io 로그인
2. DevTools → Application → Cookies → `https://velog.io`
3. **`access_token`** (1시간 유효) 과 **`refresh_token`** (30일 유효) 값 모두 복사

> 토큰은 로그인 세션 전체에 접근 가능한 자격증명이다. 노출되지 않도록 주의.

### 자동 재발급

`VELOG_REFRESH_TOKEN`을 함께 넣어두면 access_token이 만료(또는 만료 30분 이내)될 때
velog 서버가 응답의 `Set-Cookie`로 새 access_token을 내려준다. 이 서버는 이를 메모리에 흡수해
같은 프로세스에서는 그 뒤로도 계속 인증된 요청을 보낸다.

프로세스 재시작 후에도 갱신된 토큰을 보존하려면 `VELOG_TOKEN_FILE`에 영속화 경로를 지정한다.
파일이 존재하면 시작 시 env보다 우선 적용된다.

## Claude Code에 등록

```bash
claude mcp add velog \
  -e VELOG_ACCESS_TOKEN=<access_token> \
  -e VELOG_REFRESH_TOKEN=<refresh_token> \
  -e VELOG_TOKEN_FILE=$HOME/.config/velog-mcp/tokens.json \
  -- /Users/seonwoo_jung/workspace/velog-mcp-go/velog-mcp-go
```

확인:

```bash
claude mcp list
claude mcp get velog
```

## 직접 호출 테스트 (CLI 디버깅용)

```bash
# tools/list
(printf '%s\n%s\n%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"cli","version":"0"}}}' \
  '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
  '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'; sleep 1) \
| ./velog-mcp-go
```

## 환경 변수

| 변수 | 기본값 | 설명 |
|------|--------|------|
| `VELOG_ACCESS_TOKEN` | (없음) | `access_token` 쿠키. 인증 도구 사용 시 (refresh_token만 줘도 자동 발급 가능) |
| `VELOG_REFRESH_TOKEN` | (없음) | `refresh_token` 쿠키. 있으면 만료된 access_token을 서버가 자동 갱신해 응답 |
| `VELOG_TOKEN_FILE` | (없음) | 갱신된 토큰을 영속화할 JSON 경로. 존재 시 시작 때 env보다 우선 |
| `VELOG_ENDPOINT` | `https://v3.velog.io/graphql` | GraphQL endpoint 오버라이드 |

## TypeScript 원본과의 차이

- 언어/런타임만 다르고 동작은 동일하다.
- Schema는 Go의 struct 태그(`json`, `jsonschema`)로부터 자동 추론된다. 즉, TypeScript의 zod 정의를 그대로 옮긴 셈.
- `limit` 기본값(20)과 `is_markdown=true`는 핸들러에서 보강한다.
- `tags`, `meta`는 누락된 경우 빈 배열/객체로 채워서 보낸다 (zod default 동치).

## 주의

- Velog GraphQL은 **비공식**이라 스펙이 예고 없이 바뀔 수 있다.
- 삭제 mutation은 공개 스키마에 노출되지 않아 이 서버는 지원하지 않는다 (Velog 웹에서 직접 처리).
- `velog_edit_post`는 GraphQL 특성상 모든 필수 필드를 다시 보내야 한다. 일부만 바꾸려면 `velog_get_post`로 현재 값을 가져와 머지한 뒤 호출.
