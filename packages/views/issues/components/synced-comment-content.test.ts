// @vitest-environment node
import { describe, expect, it } from "vitest";
import { syncedCommentContent } from "./synced-comment-content";

const source = "https://www.tapd.cn/61188658/bugtrace/bugs/view/1161188658001004433";
const receipt = "[SaveReview:9add46d2-76e3-41d8-bc66-1179e146544c:tapd:1161188658001007147]";
const body = "毛毛 · 2026-10-12 15:58:10\n在流转 [新-接受/处理] 添加\n\n原始说明\n\n![证据](/api/attachments/proof/download)\n\n/note is discussed here";
const original = `/note\n\n${body}\n\n来源：${source}\n${receipt}`;

describe("syncedCommentContent", () => {
  it("compacts only the integration envelope and leaves evidence and authored text intact", () => {
    expect(syncedCommentContent(original)).toBe(`${body}\n\n[TAPD](${source})`);
    expect(original).toContain(receipt);
    expect(original).toMatch(/^\/note/);
  });

  it.each([
    "/note ordinary user note",
    `/note\n\nAuthor text\n\n来源：${source}\n${receipt}`,
    `Quoted example:\n\n${original}`,
    `\`\`\`\n${original}\n\`\`\``,
    original.replace(":tapd:", ":execution:"),
    original.replace("www.tapd.cn", "other.example"),
    original.replace(receipt, "[SaveReview:unknown:tapd:123]"),
    `${original}\nHuman addition`,
  ])("preserves unmatched or edited content verbatim: %s", (content) => {
    expect(syncedCommentContent(content)).toBe(content);
  });
});
