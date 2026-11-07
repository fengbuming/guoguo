import { $, api, post } from './ui-core.js';

export function createPlaybackSettings() {
  let settings = null;
  const fields = ['maxSessions', 'maxMediaRequests', 'maxRemuxJobs', 'maxAudioTranscodes', 'maxVideoTranscodes', 'maxPrefetchJobs', 'cacheMB'];
  function updateDirect() {
    const enabled = $('originalPlaybackEnabled').checked;
    $('playbackDirectSources').querySelectorAll('input').forEach(input => {input.disabled = !enabled;});
    $('playbackDirectHint').textContent = enabled
      ? '允许尝试直连，不保证每集生效。需要服务器解密、转码，或源站鉴权、HTTPS、跨域检查未通过时仍走中转；客户端连接失败也会自动回退。'
      : '原始 MP4 / HLS 播放已关闭，已勾选的直连站源也不会生效。先开启上方开关，再保存播放设置。';
  }
  function render(result) {
    settings = result.settings;
    $('originalPlaybackEnabled').checked = settings.enabled;
    for (const name of fields) $('playback-' + name).value = settings[name];
    const active = Number(result.activeSessions) || 0;
    const limit = Number(settings.maxSessions) || 0;
    $('playbackSessionsStatus').textContent = '当前占用播放会话：' + active + (limit > 0 ? ' / ' + limit : ' / 不限制') + '。清理会中断当前所有播放，页面会自动重连。';
    const sources = $('playbackDirectSources');
    sources.replaceChildren();
    for (const source of result.sources || []) {
      const label = document.createElement('label');
      const input = document.createElement('input');
      input.type = 'checkbox';
      input.value = source.id;
      input.checked = (settings.directSources || []).includes(source.id);
      label.append(input, document.createTextNode(source.name));
      sources.appendChild(label);
    }
    updateDirect();
  }
  async function load() {
    try {render(await api('/api/ui/admin/playback'));}
    catch (error) {$('playbackSettingsStatus').textContent = error.message;}
  }
  async function save() {
    if (!settings) return;
    const button = $('savePlaybackSettings');
    button.disabled = true;
    try {
      const value = {...settings, enabled: $('originalPlaybackEnabled').checked,
        directSources: [...$('playbackDirectSources').querySelectorAll('input:checked')].map(input => input.value)};
      for (const name of fields) value[name] = Number($('playback-' + name).value);
      await post('/api/ui/admin/playback', value);
      await load();
      $('playbackSettingsStatus').textContent = '播放设置已保存，新任务按当前限制执行。' + (!value.enabled && value.directSources.length ? '原始播放已关闭，直连暂不生效。' : '');
    } catch (error) {$('playbackSettingsStatus').textContent = '保存失败：' + error.message;}
    finally {button.disabled = false;}
  }
  async function cleanup() {
    const button = $('resetPlaybackSessionsBtn');
    button.disabled = true;
    try {
      const result = await post('/api/ui/admin/playback/cleanup', {});
      await load();
      $('playbackSettingsStatus').textContent = '已清理 ' + (Number(result.cleared) || 0) + ' 个播放会话；正在播放的页面会自动重连。';
    } catch (error) {$('playbackSettingsStatus').textContent = '清理失败：' + error.message;}
    finally {button.disabled = false;}
  }
  function init() {$('savePlaybackSettings').onclick = save; $('resetPlaybackSessionsBtn').onclick = cleanup; $('originalPlaybackEnabled').onchange = updateDirect; return load();}
  return {init};
}
