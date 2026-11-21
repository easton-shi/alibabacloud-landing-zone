package com.aliyun.autowonder.executor;

import com.aliyun.autowonder.common.error.BizException;
import com.aliyun.autowonder.common.error.ErrorCode;
import com.aliyun.autowonder.common.i18n.SystemMessages;
import com.aliyun.autowonder.executor.dto.ExecutorLaunchOptionsVO;
import com.aliyun.autowonder.executor.dto.ProviderModelCatalogItemVO;
import com.aliyun.autowonder.executor.dto.ProviderModelCatalogVO;
import com.aliyun.autowonder.executor.dto.SelectOptionVO;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.stereotype.Service;

import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Locale;
import java.util.Map;

/**
 * Single server-side source of truth for the values the executor page lets an operator pick.
 * Mirrors frontend/src/features/executor/qoderOptions.ts so an MCP caller sees exactly the same
 * choices, labels and defaults as the page instead of hardcoding them.
 */
@Service
public class ExecutorLaunchOptionsService {

    private static final Logger log = LoggerFactory.getLogger(ExecutorLaunchOptionsService.class);

    public static final String CLIENT_KIND_QODER_CLI = "QODER_CLI";
    public static final String CLIENT_KIND_QODER_CN_CLI = "QODER_CN_CLI";
    public static final String CLIENT_KIND_CLAUDE_CODE = "CLAUDE_CODE";
    public static final String CLIENT_KIND_CODEX_CLI = "CODEX_CLI";
    public static final String CLIENT_KIND_OPENCODE_CLI = "OPENCODE_CLI";
    public static final String CLIENT_KIND_PI_CLI = "PI_CLI";
    public static final String CLIENT_KIND_CURSOR_CLI = "CURSOR_CLI";
    public static final String PROVIDER_QODER = "qoder";
    public static final String PROVIDER_QODER_CN = "qodercn";
    public static final String PROVIDER_CLAUDE = "claude";
    public static final String PROVIDER_CODEX = "codex";
    public static final String PROVIDER_OPENCODE = "opencode";
    public static final String PROVIDER_PI = "pi";
    public static final String PROVIDER_CURSOR = "cursor";

    public static final String MEMORY_MODE_PLATFORM = "platform";
    public static final String MEMORY_MODE_PROVIDER_LOCAL = "provider-local";
    public static final String MEMORY_MODE_NONE = "none";
    public static final String DEFAULT_MEMORY_MODE = MEMORY_MODE_PLATFORM;

    public static final String AUTO_MODEL = "auto";
    /**
     * The literal the page pre-fills before it validates against the live catalog. Usable model ids are rotated
     * in Redis, so this is only a hint: {@link #chooseModel} demotes it once the catalog stops offering it.
     */
    public static final String DEFAULT_MODEL = "qmodel_latest";
    public static final String DEFAULT_CONTEXT_WINDOW = "260000";
    public static final String DEFAULT_REASONING_EFFORT = "medium";
    public static final String ULTIMATE_MODEL = "ultimate";
    public static final String ULTIMATE_REASONING_EFFORT = "high";

    /**
     * One table for every client kind the server knows how to launch. A kind is only creatable when the bundled
     * runtime has a matching provider and the server has a complete launch path for it; CURSOR_CLI stays readable
     * for historical rows but is deliberately not creatable because the runtime has no Cursor provider.
     */
    private static final List<ProviderDefinition> PROVIDER_DEFINITIONS = List.of(
            new ProviderDefinition(CLIENT_KIND_QODER_CLI, PROVIDER_QODER, "Qoder CLI",
                    "executor.option.client.qoder_cli.description", "国际版 Qoder CLI 执行器", true, true, true),
            new ProviderDefinition(CLIENT_KIND_QODER_CN_CLI, PROVIDER_QODER_CN, "Qoder CLI CN",
                    "executor.option.client.qoder_cn_cli.description", "国内版 Qoder CLI 执行器", true, true, true),
            new ProviderDefinition(CLIENT_KIND_CLAUDE_CODE, PROVIDER_CLAUDE, "Claude Code",
                    "executor.option.client.claude_code.description", "Claude Code 执行器", true, false, false),
            new ProviderDefinition(CLIENT_KIND_CODEX_CLI, PROVIDER_CODEX, "Codex CLI",
                    "executor.option.client.codex_cli.description", "Codex CLI 执行器", true, false, false),
            new ProviderDefinition(CLIENT_KIND_OPENCODE_CLI, PROVIDER_OPENCODE, "OpenCode CLI",
                    "executor.option.client.opencode_cli.description", "OpenCode CLI 执行器", true, false, false),
            new ProviderDefinition(CLIENT_KIND_PI_CLI, PROVIDER_PI, "Pi",
                    "executor.option.client.pi_cli.description", "Pi 执行器", true, false, false),
            new ProviderDefinition(CLIENT_KIND_CURSOR_CLI, PROVIDER_CURSOR, "Cursor CLI",
                    "executor.option.client.cursor_cli.description", "历史 Cursor CLI 执行器（暂不支持新建）", false, false, false));

    private static final Map<String, ProviderDefinition> PROVIDER_BY_CLIENT_KIND = buildProviderMap();
    private static final List<SelectOptionVO> CREATABLE_CLIENT_KINDS = PROVIDER_DEFINITIONS.stream()
            .filter(ProviderDefinition::creatable)
            .map(definition -> new SelectOptionVO(definition.clientKind(), definition.label(), definition.description()))
            .toList();

    private static final List<SelectOptionVO> MEMORY_MODES = List.of(
            new SelectOptionVO(MEMORY_MODE_PLATFORM, "平台记忆（推荐）", "由 AutoWonder 注入平台记忆"),
            new SelectOptionVO(MEMORY_MODE_PROVIDER_LOCAL, "本机 Agent 记忆", "使用执行器本机的 Agent 记忆"),
            new SelectOptionVO(MEMORY_MODE_NONE, "关闭记忆", "不注入任何记忆"));

    private static final List<SelectOptionVO> CONTEXT_WINDOWS = List.of(
            SelectOptionVO.of("1000000", "1M"),
            SelectOptionVO.of("400000", "400K"),
            SelectOptionVO.of("260000", "260K"));

    private static final List<SelectOptionVO> REASONING_EFFORTS = List.of(
            SelectOptionVO.of("max", "Max"),
            SelectOptionVO.of("xhigh", "Extra High"),
            SelectOptionVO.of("high", "High"),
            SelectOptionVO.of("medium", "Medium"),
            SelectOptionVO.of("low", "Low"),
            SelectOptionVO.of("none", "None"));

    /** Used only while no live provider catalog snapshot exists, exactly like the page's fallback. */
    private static final List<SelectOptionVO> FALLBACK_MODELS = List.of(
            SelectOptionVO.of(AUTO_MODEL, "Auto (default)"),
            SelectOptionVO.of(ULTIMATE_MODEL, "Ultimate"),
            SelectOptionVO.of("performance", "Performance"),
            SelectOptionVO.of("efficient", "Efficient"),
            SelectOptionVO.of("lite", "Lite"),
            SelectOptionVO.of("qmodel_38max", "Qwen3.8-Max"),
            SelectOptionVO.of("qfmodel", "Qwen3.8-Flash"),
            SelectOptionVO.of(DEFAULT_MODEL, "Qwen3.7-Max"),
            SelectOptionVO.of("qmodel", "Qwen3.7-Plus"),
            SelectOptionVO.of("kmodel_latest", "Kimi-K3"),
            SelectOptionVO.of("kmodel", "Kimi-K2.7-Code"),
            SelectOptionVO.of("gmodel", "GLM-5.3"),
            SelectOptionVO.of("gfmodel", "GLM-5.3-Flash"),
            SelectOptionVO.of("dmodel", "DeepSeek-V4-Pro"),
            SelectOptionVO.of("dfmodel", "DeepSeek-V4-Flash"),
            SelectOptionVO.of("mmodel", "MiniMax-M3"));

    private final ProviderModelCatalogService modelCatalogService;

    public ExecutorLaunchOptionsService(ProviderModelCatalogService modelCatalogService) {
        this.modelCatalogService = modelCatalogService;
    }

    public static List<SelectOptionVO> creatableClientKinds() {
        return CREATABLE_CLIENT_KINDS.stream().map(option -> {
            ProviderDefinition definition = PROVIDER_BY_CLIENT_KIND.get(option.getValue());
            return new SelectOptionVO(option.getValue(), option.getLabel(),
                    SystemMessages.render(definition.descriptionKey(), null, option.getDescription()));
        }).toList();
    }

    public static List<String> creatableClientKindValues() {
        return values(CREATABLE_CLIENT_KINDS);
    }

    public static List<String> memoryModeValues() {
        return values(MEMORY_MODES);
    }

    public static List<String> contextWindowValues() {
        return values(CONTEXT_WINDOWS);
    }

    public static List<String> reasoningEffortValues() {
        return values(REASONING_EFFORTS);
    }

    public static List<SelectOptionVO> fallbackModels() {
        return FALLBACK_MODELS.stream().map(option -> SelectOptionVO.of(option.getValue(),
                List.of("auto", "ultimate", "performance", "efficient", "lite").contains(option.getValue())
                        ? SystemMessages.render("executor.option.model." + option.getValue(), null, option.getLabel())
                        : option.getLabel())).toList();
    }

    public static String resolveProvider(String clientKind) {
        ProviderDefinition definition = clientKind == null ? null : PROVIDER_BY_CLIENT_KIND.get(clientKind.trim().toUpperCase(Locale.ROOT));
        return definition == null ? PROVIDER_CLAUDE : definition.provider();
    }

    public static boolean isQoderFamily(String provider) {
        return PROVIDER_QODER.equals(provider) || PROVIDER_QODER_CN.equals(provider);
    }

    /** Returns the canonical client kind, or null when the page would not offer it. */
    public static String canonicalClientKind(String clientKind) {
        if (clientKind == null) {
            return null;
        }
        String upper = clientKind.trim().toUpperCase(Locale.ROOT);
        return PROVIDER_BY_CLIENT_KIND.containsKey(upper) ? upper : null;
    }

    public static boolean isCreatableClientKind(String clientKind) {
        ProviderDefinition definition = clientKind == null ? null
                : PROVIDER_BY_CLIENT_KIND.get(clientKind.trim().toUpperCase(Locale.ROOT));
        return definition != null && definition.creatable();
    }

    /** True when the server refreshes a live model catalog for this provider (currently the Qoder family only). */
    public static boolean hasModelCatalog(String provider) {
        return PROVIDER_QODER.equals(provider) || PROVIDER_QODER_CN.equals(provider);
    }

    /** Qoder requires a catalog model; other CLIs may keep their own default and accept an optional model id. */
    public static boolean requiresModel(String provider) {
        return hasModelCatalog(provider);
    }

    /** The official npm runtime supports Qoder only; the bundled runtime drives the other providers. */
    public static boolean usesOfficialNpmRuntime(String provider) {
        return hasModelCatalog(provider);
    }

    /**
     * Every value the launch-command form offers for one client kind, with the page's defaults. The live catalog is
     * read for {@code tenantId} only, so a workspace never sees another workspace's quota-limited model list.
     */
    public ExecutorLaunchOptionsVO launchOptions(long tenantId, String clientKind) {
        String canonical = requireCreatableClientKind(clientKind);
        String provider = resolveProvider(canonical);
        boolean catalogBacked = hasModelCatalog(provider);
        ProviderModelCatalogVO catalog = catalogBacked ? liveCatalog(tenantId, provider) : null;
        List<SelectOptionVO> models = catalogBacked
                ? (catalog == null ? fallbackModels() : toOptions(catalog.getModels()))
                : List.of();
        ExecutorLaunchOptionsVO vo = new ExecutorLaunchOptionsVO();
        vo.setClientKind(canonical);
        vo.setProvider(provider);
        vo.setModels(models);
        vo.setReasoningEfforts(REASONING_EFFORTS.stream().map(option -> SelectOptionVO.of(option.getValue(),
                SystemMessages.render("executor.option.reasoning." + option.getValue(), null, option.getLabel()))).toList());
        vo.setContextWindows(CONTEXT_WINDOWS.stream().map(option -> SelectOptionVO.of(option.getValue(), option.getLabel())).toList());
        vo.setMemoryModes(MEMORY_MODES.stream().map(option -> new SelectOptionVO(option.getValue(),
                SystemMessages.render("executor.option.memory." + option.getValue().replace('-', '_') + ".label", null, option.getLabel()),
                SystemMessages.render("executor.option.memory." + option.getValue().replace('-', '_') + ".description", null, option.getDescription()))).toList());
        vo.setDefaultModel(catalogBacked ? chooseModel(models, DEFAULT_MODEL) : null);
        vo.setDefaultCreateModel(catalogBacked ? chooseModel(models, AUTO_MODEL) : null);
        vo.setDefaultReasoningEffort(defaultReasoningEffort(vo.getDefaultModel()));
        vo.setDefaultContextWindow(DEFAULT_CONTEXT_WINDOW);
        vo.setDefaultMemoryMode(DEFAULT_MEMORY_MODE);
        vo.setModelCatalogLastSuccessfulAt(catalog == null ? null : catalog.getLastSuccessfulAt());
        return vo;
    }

    /**
     * The catalog Redis refreshes automatically for one workspace and provider, or null while that workspace has no
     * usable snapshot, so a caller can report "no live ids yet" instead of passing the built-in fallback list off as
     * the current one.
     */
    public ProviderModelCatalogVO liveCatalog(long tenantId, String provider) {
        if (!hasModelCatalog(provider)) {
            return null;
        }
        ProviderModelCatalogVO catalog = readCatalogQuietly(tenantId, provider);
        return catalog == null || toOptions(catalog.getModels()).isEmpty() ? null : catalog;
    }

    /**
     * The ids the workspace's catalog currently holds for one provider, in catalog order. Empty while no usable
     * snapshot exists, so a caller can tell "no live ids yet" apart from the built-in fallback list.
     */
    public List<String> liveModelIds(long tenantId, String provider) {
        ProviderModelCatalogVO catalog = liveCatalog(tenantId, provider);
        return catalog == null ? List.of() : values(toOptions(catalog.getModels()));
    }

    /** The workspace's live provider catalog when one exists, otherwise the fallback list the page also uses. */
    public List<SelectOptionVO> models(long tenantId, String provider) {
        if (!hasModelCatalog(provider)) {
            return List.of();
        }
        ProviderModelCatalogVO catalog = readCatalogQuietly(tenantId, provider);
        List<SelectOptionVO> models = catalog == null ? List.of() : toOptions(catalog.getModels());
        return models.isEmpty() ? fallbackModels() : models;
    }

    public String requireCreatableClientKind(String clientKind) {
        return requireCreatableClientKind(clientKind, ErrorCode.MCP_TOOL_ARGUMENT_INVALID);
    }

    /**
     * The one client-kind gate every create entry shares (MCP and REST), so neither can persist a null
     * or non-creatable kind. Each entry passes the error code its own callers understand.
     */
    public static String requireCreatableClientKind(String clientKind, ErrorCode invalid) {
        if (!isCreatableClientKind(clientKind)) {
            throw BizException.keyed(invalid, "executor.option.only_supported",
                    new Object[]{"clientKind", String.join("/", creatableClientKindValues())},
                    "clientKind 仅支持 " + String.join("/", creatableClientKindValues()));
        }
        return canonicalClientKind(clientKind);
    }

    public String resolveMemoryMode(String requested) {
        if (isBlank(requested)) {
            return DEFAULT_MEMORY_MODE;
        }
        String value = requested.trim();
        if (memoryModeValues().contains(value)) {
            return value;
        }
        throw invalid("memoryMode", memoryModeValues());
    }

    public String resolveContextWindow(String requested) {
        if (isBlank(requested)) {
            return DEFAULT_CONTEXT_WINDOW;
        }
        String value = requested.trim();
        if (contextWindowValues().contains(value)) {
            return value;
        }
        throw invalid("contextWindow", contextWindowValues());
    }

    public String resolveReasoningEffort(String model, String requested) {
        if (isBlank(requested)) {
            return defaultReasoningEffort(model);
        }
        String value = requested.trim();
        if (reasoningEffortValues().contains(value)) {
            return value;
        }
        throw invalid("reasoningEffort", reasoningEffortValues());
    }

    public static String defaultReasoningEffort(String model) {
        return ULTIMATE_MODEL.equals(model) ? ULTIMATE_REASONING_EFFORT : DEFAULT_REASONING_EFFORT;
    }

    /** Mirrors the page's chooseQoderModel(): keep the preferred model while it is still offered. */
    public static String chooseModel(List<SelectOptionVO> models, String preferred) {
        List<String> modelValues = values(models);
        if (preferred != null && modelValues.contains(preferred)) {
            return preferred;
        }
        if (modelValues.contains(AUTO_MODEL)) {
            return AUTO_MODEL;
        }
        return modelValues.isEmpty() ? preferred : modelValues.get(0);
    }

    private ProviderModelCatalogVO readCatalogQuietly(long tenantId, String provider) {
        if (modelCatalogService == null) {
            return null;
        }
        try {
            return modelCatalogService.read(tenantId, provider);
        } catch (RuntimeException exception) {
            log.warn("provider model catalog unavailable tenantId={} provider={}, falling back to static models",
                    tenantId, provider, exception);
            return null;
        }
    }

    private static List<SelectOptionVO> toOptions(List<ProviderModelCatalogItemVO> items) {
        List<SelectOptionVO> options = new ArrayList<>();
        if (items == null) {
            return options;
        }
        for (ProviderModelCatalogItemVO item : items) {
            if (item == null || isBlank(item.getId())) {
                continue;
            }
            String label = isBlank(item.getName()) ? item.getId() : item.getName();
            options.add(SelectOptionVO.of(item.getId(), label));
        }
        return options;
    }

    private static List<String> values(List<SelectOptionVO> options) {
        List<String> values = new ArrayList<>(options.size());
        for (SelectOptionVO option : options) {
            values.add(option.getValue());
        }
        return List.copyOf(values);
    }

    private static boolean isBlank(String value) {
        return value == null || value.isBlank();
    }

    private static BizException invalid(String field, List<String> allowed) {
        return BizException.keyed(ErrorCode.MCP_TOOL_ARGUMENT_INVALID, "executor.option.only_supported",
                new Object[]{field, String.join("/", allowed)}, field + " 仅支持 " + String.join("/", allowed));
    }

    private static Map<String, ProviderDefinition> buildProviderMap() {
        Map<String, ProviderDefinition> definitions = new LinkedHashMap<>();
        for (ProviderDefinition definition : PROVIDER_DEFINITIONS) {
            definitions.put(definition.clientKind(), definition);
        }
        return Map.copyOf(definitions);
    }

    private record ProviderDefinition(String clientKind, String provider, String label, String descriptionKey,
                                      String description, boolean creatable, boolean qoderFamily,
                                      boolean catalogBacked) {
    }
}
