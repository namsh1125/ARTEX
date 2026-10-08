import assert from "node:assert/strict";
import test from "node:test";
import { activeMention, mentionSearch, mentionToken, selectedMentions } from "./chat-mentions.ts";

test("mention trigger supports Chinese and cursor placement without hijacking email", () => {
  assert.equal(activeMention("user@example.com", 16), null);
  assert.equal(activeMention("已选 @[漏洞#1 X]", 12), null);
  assert.deepEqual(activeMention("查看@漏洞 后面的文字", 5), { start: 2, end: 5, query: "漏洞" });
  assert.equal(activeMention("@漏洞\n下一行", 8), null);
});

test("categories, Chinese aliases, IP and keyword search", () => {
  assert.equal(mentionSearch("").categories.length, 9);
  assert.equal(mentionSearch("漏").categories[0].kind, "finding");
  assert.equal(mentionSearch("漏洞").kind, "finding");
  assert.equal(mentionSearch("漏洞SQL注入").query, "SQL注入");
  assert.equal(mentionSearch("ip 192.0.2.1").kind, "ip");
  assert.equal(mentionSearch("接口 GET /api").query, "GET /api");
  assert.equal(mentionSearch("acme.com").kind, "");
});

test("tokens roundtrip labels and removing one reference preserves its neighbors", () => {
  const first = mentionToken({ kind: "finding", id: 12, label: "제목[1]\n설명" });
  const second = mentionToken({ kind: "ip", id: 13, label: "192.0.2.1" });
  const value = `${first} 및 ${second} 분석`;
  const selected = selectedMentions(value);
  assert.equal(selected.length, 2);
  assert.equal(selected[0].label, "취약점 #12 · 제목（1） 설명");
  const next = value.slice(0, selected[0].start) + value.slice(selected[0].start + selected[0].token.length);
  assert.equal(selectedMentions(next)[0].token, second);
});

// Korean display/search must keep the backend's existing mention wire format.
test("Korean mentions preserve backend tokens and display localized categories", () => {
  assert.deepEqual(activeMention("확인 @취약점", 7), { start: 3, end: 7, query: "취약점" });
  assert.equal(mentionSearch("취").categories[0].kind, "finding");
  assert.equal(mentionSearch("취약점 SQL 인젝션").query, "SQL 인젝션");
  assert.equal(mentionSearch("하위 도메인 example.com").kind, "subdomain");
  assert.equal(mentionSearch("finding SQL").query, "SQL");
  const token = mentionToken({ kind: "finding", id: 12, label: "검증 결과" });
  assert.equal(token, "@[漏洞#12 검증 결과]");
  assert.equal(selectedMentions(token)[0].label, "취약점 #12 · 검증 결과");
});
