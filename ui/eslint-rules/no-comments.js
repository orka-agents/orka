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
        for (const comment of sourceCode.getAllComments()) {
          if (comment.type === "Shebang" || directive.test(comment.value)) {
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
