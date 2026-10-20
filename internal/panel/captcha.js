'use strict';
/* 阿里云无痕验证（滑块）求解器：AutoClaw 国际版 OAuth 的前置风控。
 *
 * SDK 只从 o.alicdn.com 加载；页面 CSP 已按 index.go 的说明显式放行。
 * solve(config, request) 在用户完成滑块后，把 SDK 产出的不透明 verifyParam
 * 交给 request（面板用它换 OAuth 授权地址），request 的返回值即 solve 的结果。
 * 一次只允许一个验证码流程。 */

const AliyunCaptcha = (() => {
  const SCRIPT_URL = 'https://o.alicdn.com/captcha-frontend/aliyunCaptcha/AliyunCaptcha.js';
  const SCRIPT_ID = 'aliyun-captcha-sdk';
  const ELEMENT_ID = 'aliyun-captcha-element';
  const BUTTON_ID = 'aliyun-captcha-trigger';
  const SCRIPT_TIMEOUT_MS = 40000;
  const INIT_TIMEOUT_MS = 40000;
  const VERIFY_TIMEOUT_MS = 120000;
  const WARMUP_MS = 2100;

  let scriptPromise = null;
  let initPromise = null;
  let initKey = '';
  let instance = null;
  let initializedAt = 0;
  let pending = null;

  const sleep = ms => new Promise(resolve => setTimeout(resolve, ms));

  function ensureElements() {
    let el = document.getElementById(ELEMENT_ID);
    if (!el) {
      el = document.createElement('div');
      el.id = ELEMENT_ID;
      el.style.position = 'relative';
      el.style.zIndex = '2147483000';
      document.body.appendChild(el);
    }
    let btn = document.getElementById(BUTTON_ID);
    if (!btn) {
      btn = document.createElement('button');
      btn.id = BUTTON_ID;
      btn.type = 'button';
      btn.tabIndex = -1;
      btn.setAttribute('aria-hidden', 'true');
      btn.style.position = 'fixed';
      btn.style.width = '1px';
      btn.style.height = '1px';
      btn.style.opacity = '0';
      btn.style.pointerEvents = 'none';
      btn.style.overflow = 'hidden';
      document.body.appendChild(btn);
    }
    return btn;
  }

  function loadScript(config) {
    window.AliyunCaptchaConfig = { region: config.region, prefix: config.prefix };
    if (typeof window.initAliyunCaptcha === 'function') return Promise.resolve();
    if (scriptPromise) return scriptPromise;
    scriptPromise = new Promise((resolve, reject) => {
      const existing = document.getElementById(SCRIPT_ID);
      const script = existing || document.createElement('script');
      let timer = 0;
      const cleanup = () => {
        clearTimeout(timer);
        script.removeEventListener('load', onLoad);
        script.removeEventListener('error', onError);
      };
      const fail = msg => {
        cleanup();
        scriptPromise = null;
        if (script.parentNode) script.parentNode.removeChild(script);
        reject(new Error(msg));
      };
      const onLoad = () => {
        cleanup();
        if (typeof window.initAliyunCaptcha === 'function') resolve();
        else fail('验证码组件加载异常，请重试');
      };
      const onError = () => fail('验证码组件加载失败，请检查网络后重试');
      timer = setTimeout(() => fail('验证码组件加载超时，请检查网络后重试'), SCRIPT_TIMEOUT_MS);
      script.addEventListener('load', onLoad);
      script.addEventListener('error', onError);
      if (!existing) {
        script.id = SCRIPT_ID;
        script.async = true;
        script.src = SCRIPT_URL;
        document.head.appendChild(script);
      }
    });
    return scriptPromise;
  }

  function failPending(msg) {
    const p = pending;
    if (!p) return;
    pending = null;
    clearTimeout(p.timer);
    p.reject(new Error(msg || '验证码校验失败，请重试'));
  }

  function captchaVerify(param) {
    const p = pending;
    if (!p) return Promise.resolve({ captchaResult: false, bizResult: false });
    if (typeof param !== 'string' || !param) {
      failPending('验证码校验失败，请重试');
      return Promise.resolve({ captchaResult: false, bizResult: false });
    }
    return Promise.resolve(p.request(param)).then(result => {
      if (pending === p) {
        pending = null;
        clearTimeout(p.timer);
        p.resolve(result);
      }
      return { captchaResult: true, bizResult: true };
    }).catch(err => {
      // 业务失败 ≠ 滑块失败：captchaResult 必须如实回报 true（滑块已通过），
      // 只把 bizResult 置 false——否则阿里云 SDK 会把后端错误当成"验证失败，
      // 请刷新"无限循环，用户永远看不到真实原因（实测 qZTAw7vhEb 闭环）。
      failPending((err && err.message) || '登录失败，请重试');
      return { captchaResult: true, bizResult: false };
    });
  }

  function init(config) {
    const key = [config.region || '', config.prefix || '', config.sceneId || ''].join(':');
    if (initPromise && initKey === key) return initPromise;
    if (initPromise && initKey !== key) {
      try { if (instance && instance.destroy) instance.destroy(); } catch (e) { /* ignore */ }
      initPromise = null;
      instance = null;
    }
    initKey = key;
    initPromise = (async () => {
      await loadScript(config);
      ensureElements();
      const initFn = window.initAliyunCaptcha;
      if (typeof initFn !== 'function') throw new Error('验证码组件不可用，请重试');
      await new Promise((resolve, reject) => {
        let settled = false;
        const timer = setTimeout(() => {
          if (settled) return;
          settled = true;
          reject(new Error('验证码组件初始化超时，请重试'));
        }, INIT_TIMEOUT_MS);
        const finish = (ok, err) => {
          if (settled) return;
          settled = true;
          clearTimeout(timer);
          if (ok) resolve(); else reject(err);
        };
        try {
          initFn({
            SceneId: config.sceneId,
            mode: 'popup',
            element: '#' + ELEMENT_ID,
            button: '#' + BUTTON_ID,
            captchaVerifyCallback: param => captchaVerify(param),
            onBizResultCallback: () => {},
            getInstance: inst => {
              instance = inst;
              initializedAt = Date.now();
              finish(true);
            },
            slideStyle: { width: 360, height: 40 },
            language: 'cn',
            onError: err => {
              const msg = (err && err.message) || '验证码组件不可用，请重试';
              if (pending) failPending(msg);
              else finish(false, new Error(msg));
            }
          });
        } catch (err) {
          finish(false, new Error((err && err.message) || '验证码组件不可用，请重试'));
        }
      });
      if (!instance) throw new Error('验证码组件不可用，请重试');
    })();
    initPromise.catch(() => { initPromise = null; });
    return initPromise;
  }

  async function solve(config, request) {
    if (pending) throw new Error('已有验证码流程进行中，请稍候');
    if (!config || !config.scene_id) throw new Error('上游未返回风控配置');
    await init({ region: config.region, prefix: config.prefix, sceneId: config.scene_id });
    const wait = WARMUP_MS - (Date.now() - initializedAt);
    if (wait > 0) await sleep(wait);
    const btn = ensureElements();
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => failPending('验证码校验超时，请重试'), VERIFY_TIMEOUT_MS);
      pending = { request, resolve, reject, timer };
      btn.click();
    });
  }

  function cancel() {
    failPending('已取消验证码');
  }

  return { solve: solve, cancel: cancel };
})();
