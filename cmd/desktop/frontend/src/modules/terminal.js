import {
  DetectTerminals,
  GetTerminalConfig,
  SaveTerminalConfig,
  AddProjectDir,
  RemoveProjectDir,
  LaunchTerminal,
  LaunchSessionTerminal,
  LaunchCodexTerminal,
  LaunchCodexSessionTerminal,
  LaunchGeminiTerminal,
  LaunchGeminiSessionTerminal,
  SelectDirectory,
  ApplyCLIConfig,
  RestoreCLIConfigs,
  GetMatchingEndpointsForCLI,
  IsCLIConfigModified,
} from "../../wailsjs/go/main/App";
import { t } from "../i18n/index.js";
import { showNotification } from "./modal.js";
import {
  getSelectedSession,
  clearSelectedSession,
  clearAllSelectedSessions,
} from "./session.js";

// 翻译后端错误消息
function translateError(error) {
  const errorStr = error.toString();
  const errorKey = `terminal.errors.${errorStr}`;
  const translated = t(errorKey);
  return translated !== errorKey ? translated : errorStr;
}

let terminals = [];
let terminalConfig = { selectedTerminal: "cmd", projectDirs: [] };
let currentCliType = "claude"; // 'claude' | 'codex' | 'gemini'

// 获取当前 CLI 类型
export function getCurrentCliType() {
  return currentCliType;
}

export function initTerminal() {
  window.showTerminalModal = showTerminalModal;
  window.closeTerminalModal = closeTerminalModal;
  window.onTerminalChange = onTerminalChange;
  window.onClaudeCommandChange = onClaudeCommandChange;
  window.onGeminiCommandChange = onGeminiCommandChange;
  window.addProjectDir = addProjectDir;
  window.removeProjectDir = removeProjectDir;
  window.launchTerminal = launchTerminal;
  window.switchCliType = switchCliType;

  // 监听会话选择事件，更新界面
  window.addEventListener("sessionSelected", () => {
    renderProjectDirs();
  });
}

function switchCliType(cliType) {
  currentCliType = cliType;
  // 更新按钮样式
  document.querySelectorAll(".cli-type-btn").forEach((btn) => {
    btn.classList.toggle("active", btn.dataset.cli === cliType);
  });
  // 更新帮助文本和命令输入框显示
  const helpText = document.getElementById("terminalSelectHelp");
  const claudeCmdGroup = document.getElementById("claudeCommandGroup");
  const geminiCmdGroup = document.getElementById("geminiCommandGroup");
  if (helpText) {
    if (cliType === "claude") {
      helpText.textContent = t("terminal.selectTerminalHelp");
      if (claudeCmdGroup) claudeCmdGroup.style.display = "block";
      if (geminiCmdGroup) geminiCmdGroup.style.display = "none";
    } else if (cliType === "codex") {
      helpText.textContent = t("terminal.selectTerminalHelpCodex");
      if (claudeCmdGroup) claudeCmdGroup.style.display = "none";
      if (geminiCmdGroup) geminiCmdGroup.style.display = "none";
    } else if (cliType === "gemini") {
      helpText.textContent = t("terminal.selectTerminalHelpGemini");
      if (claudeCmdGroup) claudeCmdGroup.style.display = "none";
      if (geminiCmdGroup) geminiCmdGroup.style.display = "block";
    }
  }
  // 应用CLI配置并显示匹配端点
  updateCLIConfigStatus(cliType);
  // 切换时清除所有已选会话
  clearAllSelectedSessions();
  // 重新渲染项目目录
  renderProjectDirs();
}

async function updateCLIConfigStatus(cliType) {
  const configStatus = document.getElementById("cliConfigStatus");
  const matchingEndpoints = document.getElementById("matchingEndpoints");
  if (!configStatus) return;

  try {
    // 应用CLI配置
    await ApplyCLIConfig(cliType);

    // 获取匹配的端点
    const endpointsData = await GetMatchingEndpointsForCLI(cliType);
    const endpoints = JSON.parse(endpointsData);

    // 检查配置是否已被修改
    const isModified = await IsCLIConfigModified(cliType);

    // 更新配置状态显示
    const cliTypeNames = { claude: "Claude Code", codex: "Codex", gemini: "Gemini CLI" };
    if (isModified) {
      configStatus.innerHTML = `<span class="config-applied">✓ ${cliTypeNames[cliType]} 配置已应用</span>`;
      configStatus.className = "cli-config-status applied";
    } else {
      configStatus.innerHTML = `<span class="config-pending">○ 等待应用 ${cliTypeNames[cliType]} 配置</span>`;
      configStatus.className = "cli-config-status pending";
    }

    // 更新匹配端点显示
    if (matchingEndpoints) {
      if (endpoints && endpoints.length > 0) {
        const endpointNames = endpoints.map(ep => ep.name).join(", ");
        matchingEndpoints.innerHTML = `<span class="matching-ep">匹配的端点: ${endpointNames}</span>`;
        matchingEndpoints.style.display = "block";
      } else {
        matchingEndpoints.innerHTML = `<span class="no-matching-ep">没有找到匹配的端点（请确保已启用对应转换器的端点）</span>`;
        matchingEndpoints.style.display = "block";
      }
    }
  } catch (err) {
    console.error("Failed to update CLI config status:", err);
    configStatus.innerHTML = `<span class="config-error">配置应用失败: ${err.message || err}</span>`;
    configStatus.className = "cli-config-status error";
  }
}

async function showTerminalModal() {
  const modal = document.getElementById("terminalModal");
  modal.style.display = "flex";

  // Load terminals and config
  await loadTerminals();
  await loadTerminalConfig();
  // 更新当前CLI类型的配置状态
  await updateCLIConfigStatus(currentCliType);
  renderProjectDirs();
}

function closeTerminalModal() {
  document.getElementById("terminalModal").style.display = "none";
}

async function loadTerminals() {
  try {
    const data = await DetectTerminals();
    terminals = JSON.parse(data);
    renderTerminalSelect();
  } catch (err) {
    console.error("Failed to detect terminals:", err);
  }
}

async function loadTerminalConfig() {
  try {
    const data = await GetTerminalConfig();
    terminalConfig = JSON.parse(data);
    // Update select value
    const select = document.getElementById("terminalSelect");
    if (select && terminalConfig.selectedTerminal) {
      select.value = terminalConfig.selectedTerminal;
    }
    // Update claudeCommand input
    const cmdInput = document.getElementById("claudeCommandInput");
    if (cmdInput) {
      cmdInput.value = terminalConfig.claudeCommand || "";
    }
    // Update geminiCommand input
    const geminiCmdInput = document.getElementById("geminiCommandInput");
    if (geminiCmdInput) {
      geminiCmdInput.value = terminalConfig.geminiCommand || "";
    }
  } catch (err) {
    console.error("Failed to load terminal config:", err);
  }
}

function renderTerminalSelect() {
  const select = document.getElementById("terminalSelect");
  if (!select) return;

  select.innerHTML = terminals
    .map(
      (term) =>
        `<option value="${term.id}" ${term.id === terminalConfig.selectedTerminal ? "selected" : ""}>${term.name}</option>`,
    )
    .join("");
}

async function onTerminalChange() {
  const select = document.getElementById("terminalSelect");
  terminalConfig.selectedTerminal = select.value;
  try {
    await SaveTerminalConfig(
      terminalConfig.selectedTerminal,
      terminalConfig.projectDirs,
      terminalConfig.claudeCommand || "",
      terminalConfig.geminiCommand || "",
    );
  } catch (err) {
    console.error("Failed to save terminal config:", err);
  }
}

async function onClaudeCommandChange() {
  const cmdInput = document.getElementById("claudeCommandInput");
  terminalConfig.claudeCommand = cmdInput ? cmdInput.value.trim() : "";
  try {
    await SaveTerminalConfig(
      terminalConfig.selectedTerminal,
      terminalConfig.projectDirs,
      terminalConfig.claudeCommand,
      terminalConfig.geminiCommand || "",
    );
  } catch (err) {
    console.error("Failed to save claude command:", err);
  }
}

async function onGeminiCommandChange() {
  const cmdInput = document.getElementById("geminiCommandInput");
  terminalConfig.geminiCommand = cmdInput ? cmdInput.value.trim() : "";
  try {
    await SaveTerminalConfig(
      terminalConfig.selectedTerminal,
      terminalConfig.projectDirs,
      terminalConfig.claudeCommand || "",
      terminalConfig.geminiCommand,
    );
  } catch (err) {
    console.error("Failed to save gemini command:", err);
  }
}

function renderProjectDirs() {
  const container = document.getElementById("projectDirList");
  if (!container) return;

  if (!terminalConfig.projectDirs || terminalConfig.projectDirs.length === 0) {
    container.innerHTML = `<div class="empty-tip">${t("terminal.noDirs")}</div>`;
    return;
  }

  container.innerHTML = terminalConfig.projectDirs
    .map((dir, index) => {
      const selectedSession = getSelectedSession(dir);
      const hasSession = selectedSession !== null;
      const sessionName = hasSession
        ? selectedSession.info?.alias ||
          selectedSession.info?.summary ||
          selectedSession.sessionId.substring(0, 8)
        : "";
      const sessionTooltip = hasSession
        ? `已选择会话 ${selectedSession.info?.serialNumber || "-"}：${sessionName}`
        : "点击查看历史会话信息";

      // 从路径中提取项目名
      const projectName = dir.split(/[/\\]/).filter(Boolean).pop() || dir;

      return `
        <div class="project-dir-item" data-dir-index="${index}">
            <div class="dir-info">
                <span class="dir-index">${t("terminal.project")} ${index + 1}:</span>
                <span class="dir-name" title="${dir}">${projectName}</span>
            </div>
            <div class="dir-actions">
                <button class="btn btn-sm btn-primary" data-action="launch">▶ ${t("terminal.launch")}</button>
                <button class="btn btn-sm btn-danger" data-action="remove">🗑️ ${t("terminal.delete")}</button>
                <button class="btn btn-sm btn-session" data-action="session" title="${sessionTooltip}">
                    ${hasSession ? "✅" : "📋"} ${t("session.sessions")}
                    ${hasSession ? '<span class="session-clear-btn">×</span>' : ""}
                </button>
            </div>
        </div>
    `;
    })
    .join("");

  // 添加事件监听
  container.querySelectorAll(".project-dir-item").forEach((item) => {
    const dirIndex = parseInt(item.dataset.dirIndex);
    const dir = terminalConfig.projectDirs[dirIndex];
    const selectedSession = getSelectedSession(dir);
    const hasSession = selectedSession !== null;

    item.querySelector('[data-action="launch"]').onclick = () =>
      window.launchTerminal(dir);
    item.querySelector('[data-action="remove"]').onclick = () =>
      window.removeProjectDir(dir);
    item.querySelector('[data-action="session"]').onclick = () => {
      window.showSessionModal(dir);
    };

    // 添加清除会话按钮的事件
    if (hasSession) {
      const clearBtn = item.querySelector(".session-clear-btn");
      if (clearBtn) {
        clearBtn.onclick = (e) => {
          e.stopPropagation();
          clearSelectedSession(dir);
          renderProjectDirs();
        };
      }
    }
  });
}

async function addProjectDir() {
  try {
    const dir = await SelectDirectory();
    if (!dir) return;

    await AddProjectDir(dir);
    terminalConfig.projectDirs.push(dir);
    renderProjectDirs();
  } catch (err) {
    console.error("Failed to add project dir:", err);
    showNotification(translateError(err), "error");
  }
}

async function removeProjectDir(dir) {
  const confirmed = await showConfirmDialog(t("terminal.confirmDelete"));
  if (!confirmed) return;

  try {
    await RemoveProjectDir(dir);
    terminalConfig.projectDirs = terminalConfig.projectDirs.filter(
      (d) => d !== dir,
    );
    renderProjectDirs();
  } catch (err) {
    console.error("Failed to remove project dir:", err);
  }
}

function showConfirmDialog(message) {
  return new Promise((resolve) => {
    const modal = document.createElement("div");
    modal.id = "terminalConfirmModal";
    modal.className = "modal active";
    modal.style.zIndex = "1002";
    modal.innerHTML = `
            <div class="confirm-dialog-content">
                <div class="confirm-body">
                    <div class="confirm-icon">
                        <svg viewBox="0 0 24 24" fill="none" xmlns="http://www.w3.org/2000/svg">
                            <path d="M12 9v4m0 4h.01M21 12a9 9 0 11-18 0 9 9 0 0118 0z" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"/>
                        </svg>
                    </div>
                    <div class="confirm-content">
                        <h4 class="confirm-title">${t("common.confirm")}</h4>
                        <p class="confirm-message">${message}</p>
                    </div>
                </div>
                <div class="confirm-divider"></div>
                <div class="confirm-footer">
                    <button class="btn-confirm-delete" id="confirmYes">${t("common.yes")}</button>
                    <button class="btn-confirm-cancel" id="confirmNo">${t("common.no")}</button>
                </div>
            </div>
        `;
    document.body.appendChild(modal);

    modal.querySelector("#confirmYes").onclick = () => {
      modal.remove();
      resolve(true);
    };
    modal.querySelector("#confirmNo").onclick = () => {
      modal.remove();
      resolve(false);
    };
    modal.onclick = (e) => {
      if (e.target === modal) {
        modal.remove();
        resolve(false);
      }
    };
  });
}

async function launchTerminal(dir) {
  try {
    // 检查是否有选中的会话
    const selectedSession = getSelectedSession(dir);

    if (currentCliType === "codex") {
      // Codex 启动
      if (selectedSession) {
        await LaunchCodexSessionTerminal(dir, selectedSession.sessionId);
      } else {
        await LaunchCodexTerminal(dir);
      }
    } else if (currentCliType === "gemini") {
      // Gemini 启动
      if (selectedSession) {
        await LaunchGeminiSessionTerminal(dir, selectedSession.sessionId);
      } else {
        await LaunchGeminiTerminal(dir);
      }
    } else {
      // Claude Code 启动
      if (selectedSession) {
        await LaunchSessionTerminal(dir, selectedSession.sessionId);
      } else {
        await LaunchTerminal(dir);
      }
    }

    // 延时后自动关闭模态框
    setTimeout(() => closeTerminalModal(), 600);
  } catch (err) {
    console.error("Failed to launch terminal:", err);
    showNotification(
      t("terminal.launchFailed") + ": " + translateError(err),
      "error",
    );
  }
}
