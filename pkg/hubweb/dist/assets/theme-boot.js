"use strict";(()=>{var e=window.matchMedia?.("(prefers-color-scheme: light)");function t(){document.documentElement.dataset.theme=e?.matches?"light":"dark"}t();e?.addEventListener("change",t);})();
