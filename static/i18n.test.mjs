import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const I18N = require("./i18n.js");

test("zh and en dictionaries have identical key sets", () => {
  const zhKeys = Object.keys(I18N.dict.zh).sort();
  const enKeys = Object.keys(I18N.dict.en).sort();
  assert.deepEqual(enKeys, zhKeys, "zh/en 键集合必须一致");
});

test("t returns translated string and substitutes params", () => {
  I18N.setLang("zh");
  assert.equal(I18N.t("brand"), "LogViewer");
  assert.equal(I18N.t("toastConfigReloaded", { n: 5 }), "配置已重载（共 5 台机器）");
  assert.equal(I18N.t("toastReloadFailed", { msg: "boom" }), "重载失败: boom");
});

test("missing key falls back to key itself", () => {
  I18N.setLang("en");
  assert.equal(I18N.t("totally.missing.key"), "totally.missing.key");
});

test("setLang switches language", () => {
  I18N.setLang("en");
  assert.equal(I18N.lang, "en");
  assert.equal(I18N.t("refresh"), "Refresh");
  I18N.setLang("zh");
  assert.equal(I18N.t("refresh"), "刷新");
});

test("setLang ignores unknown language", () => {
  I18N.setLang("zh");
  I18N.setLang("klingon");
  assert.equal(I18N.lang, "zh");
});

test("every parameterized key declares its placeholders consistently in zh and en", () => {
  const placeholders = (s) => Array.from(s.matchAll(/\{(\w+)\}/g), (m) => m[1]).sort();
  for (const key of Object.keys(I18N.dict.zh)) {
    const zh = placeholders(I18N.dict.zh[key]);
    const en = placeholders(I18N.dict.en[key]);
    assert.deepEqual(en, zh, `占位符不一致: ${key} (zh=[${zh}] en=[${en}])`);
  }
});
