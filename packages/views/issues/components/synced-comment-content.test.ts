// @vitest-environment node
import { describe, expect, it } from "vitest";
import { parseSyncedComment, syncedCommentContent } from "./synced-comment-content";

const source = "https://www.tapd.cn/61188658/bugtrace/bugs/view/1161188658001004433";
const receipt = "[SaveReview:9add46d2-76e3-41d8-bc66-1179e146544c:tapd:1161188658001007147]";
const body = "毛毛 · 2026-10-12 15:58:10\n在流转 [新-接受/处理] 添加\n\n原始说明\n\n![证据](/api/attachments/proof/download)\n\n/note is discussed here";
const original = `/note\n\n${body}\n\n来源：${source}\n${receipt}`;

describe("syncedCommentContent", () => {
  it("compacts only the integration envelope and leaves evidence and authored text intact", () => {
    const authored = body.slice(body.indexOf("\n") + 1);
    expect(syncedCommentContent(original)).toBe(authored);
    expect(parseSyncedComment(original)).toEqual({ author: "毛毛", createdAt: "2026-10-12 15:58:10", sourceUrl: source, body: authored });
    expect(original).toContain(receipt);
    expect(original).toMatch(/^\/note/);
  });

  it("separates a source header with no title from the comment body", () => {
    const content = `/note\n\n毛毛 · 2026-10-14 21:51:58\n\n测试一下，在 TAPD\n\n来源：${source}\n${receipt}`;
    expect(syncedCommentContent(content)).toBe("测试一下，在 TAPD");
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
    expect(parseSyncedComment(content)).toBeNull();
  });
});
