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

const notice =
  /copyright|spdx-license-identifier|\blicen[cs]ed?\b|all rights reserved|permission is hereby granted|warrant|liabilit|redistribution|modified from|derived from|code generated[\s\S]*do not edit/i;

function onlyNotices(value) {
  const paragraphs = value
    .split("\n")
    .map((line) => line.replace(/^\s*\*?\s?/, ""))
    .join("\n")
    .split(/\n\s*\n/)
    .filter((paragraph) => paragraph.trim() !== "");
  return (
    paragraphs.length > 0 &&
    paragraphs.every((paragraph) => notice.test(paragraph))
  );
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
        for (const comment of sourceCode.getAllComments()) {
          const licenseHeader =
            comment.range[1] <= codeStart && onlyNotices(comment.value);
          if (
            comment.type === "Shebang" ||
            directive.test(comment.value) ||
            licenseHeader
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
