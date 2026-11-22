import { readFileSync } from "node:fs";

const directive = new RegExp(
  "^[\\s*]*(?:" +
    [
      "eslint-disable(?:-next-line|-line)?",
      "eslint-enable",
      "@ts-(?:expect-error|ignore|nocheck|check)",
      "/\\s*<reference",
      "@vite-ignore",
      "@vitest-environment",
      "(?:istanbul|c8|v8) ignore",
      "prettier-ignore",
      "[@#]__PURE__",
      "@jsx(?:ImportSource|Frag|Runtime)?",
    ].join("|") +
    ")(?=[\\s:]|$)",
);

const noticeParagraphs = JSON.parse(
  readFileSync(
    new URL("../../hack/commentlint/scan/notices.json", import.meta.url),
    "utf8",
  ),
).paragraphs.map((pattern) => new RegExp(pattern, "u"));

function onlyNotices(text) {
  const paragraphs = text
    .split(/\n\s*\n/)
    .map((paragraph) => paragraph.split(/\s+/).filter(Boolean).join(" "))
    .filter((paragraph) => paragraph !== "");
  return (
    paragraphs.length > 0 &&
    paragraphs.every((paragraph) =>
      noticeParagraphs.some((pattern) => pattern.test(paragraph)),
    )
  );
}

function plainText(comment) {
  return comment.value
    .split("\n")
    .map((line) => line.replace(/^\s*\*?\s?/, ""))
    .join("\n");
}

export const noComments = {
  meta: {
    type: "suggestion",
    docs: {
      description:
        "Disallow code comments. Name things so the code explains itself, file an issue for a known hack, or record the decision in docs/adr.",
    },
    schema: [],
    messages: {
      comment:
        "Comments are banned. Rename or restructure so the code explains itself, file an issue for a known hack, or record the decision in docs/adr.",
    },
  },
  create(context) {
    const sourceCode = context.sourceCode;
    return {
      Program() {
        const firstToken = sourceCode.ast.tokens[0];
        const codeStart = firstToken ? firstToken.range[0] : Infinity;
        const comments = sourceCode.getAllComments();
        const leading = comments.filter(
          (comment) =>
            comment.type !== "Shebang" &&
            comment.range[1] <= codeStart &&
            !directive.test(comment.value),
        );
        const header = new Set(
          onlyNotices(
            leading
              .map((comment, index) => {
                const previous = leading[index - 1];
                const gap =
                  previous && comment.loc.start.line - previous.loc.end.line > 1;
                return (gap ? "\n" : "") + plainText(comment);
              })
              .join("\n"),
          )
            ? leading
            : [],
        );
        for (const comment of comments) {
          if (
            comment.type === "Shebang" ||
            directive.test(comment.value) ||
            header.has(comment)
          ) {
            continue;
          }
          context.report({ loc: comment.loc, messageId: "comment" });
        }
      },
    };
  },
};

export default {
  rules: { "no-comments": noComments },
};
