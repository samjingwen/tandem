import js from "@eslint/js"
import tseslint from "typescript-eslint"

export default tseslint.config(
  { ignores: ["dist/**", "node_modules/**"] },
  js.configs.recommended,
  ...tseslint.configs.strictTypeChecked,
  {
    files: ["**/*.ts"],
    languageOptions: { parserOptions: { project: "./tsconfig.json", tsconfigRootDir: import.meta.dirname } },
    rules: {
      complexity: ["error", 15],
      "max-depth": ["error", 4],
      "@typescript-eslint/no-floating-promises": "error",
      "@typescript-eslint/no-misused-promises": "error",
    },
  },
  { files: ["**/*.{js,mjs}"], extends: [tseslint.configs.disableTypeChecked] },
)
