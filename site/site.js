const theme = document.querySelector('#theme');
theme.value = document.documentElement.dataset.theme || 'system';
theme.hidden = false;
theme.addEventListener('change', () => {
  document.documentElement.dataset.theme = theme.value;
  try {
    localStorage.setItem('arkex-theme', theme.value);
  } catch {}
});

const copy = document.querySelector('#copy');
const command = document.querySelector('#command');
const status = document.querySelector('#copy-status');
copy.hidden = false;
copy.addEventListener('click', async () => {
  try {
    await navigator.clipboard.writeText(command.textContent);
    status.textContent = 'Copied. Paste into your terminal.';
  } catch {
    const range = document.createRange();
    range.selectNodeContents(command);
    const selection = window.getSelection();
    selection.removeAllRanges();
    selection.addRange(range);
    status.textContent = 'Select and copy the command with Ctrl+C or ⌘C.';
  }
});
