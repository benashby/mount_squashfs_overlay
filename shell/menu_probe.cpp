// Test harness for squashoverlay_shell.dll: builds the context menu Explorer
// would show for a path and prints it, or runs one of its items.
//
//   menu_probe <dll> <path>                  print the menu
//   menu_probe <dll> <path> "Mount as" "R:"  run the item at that label path
//
// It loads the DLL directly, so no registration is needed.

#include <windows.h>
#include <shlobj.h>

#include <cstdio>
#include <string>
#include <vector>

static const CLSID CLSID_SquashoverlayMenu = {
    0x0ed6eeba, 0x7db7, 0x4de4, {0xb8, 0xb0, 0x1d, 0x35, 0x5e, 0x1e, 0xe8, 0x0d}};

static void Print(HMENU menu, int depth, UINT first) {
    for (int i = 0, n = GetMenuItemCount(menu); i < n; ++i) {
        wchar_t text[256] = {};
        MENUITEMINFOW mii = {sizeof(mii)};
        mii.fMask = MIIM_STRING | MIIM_SUBMENU | MIIM_ID | MIIM_FTYPE | MIIM_STATE;
        mii.dwTypeData = text;
        mii.cch = 255;
        GetMenuItemInfoW(menu, i, TRUE, &mii);
        if (mii.fType & MFT_SEPARATOR) {
            wprintf(L"%*s----\n", depth * 2, L"");
            continue;
        }
        wprintf(L"%*s%s%s%s\n", depth * 2, L"", text, mii.hSubMenu ? L" >" : L"",
                (mii.fState & MFS_GRAYED) ? L" (disabled)" : L"");
        if (mii.hSubMenu) Print(mii.hSubMenu, depth + 1, first);
    }
}

static bool Find(HMENU menu, const std::vector<std::wstring>& labels, size_t at, UINT* id) {
    for (int i = 0, n = GetMenuItemCount(menu); i < n; ++i) {
        wchar_t text[256] = {};
        GetMenuStringW(menu, i, text, 255, MF_BYPOSITION);
        if (labels[at] != text) continue;
        if (at + 1 == labels.size()) {
            *id = GetMenuItemID(menu, i);
            return *id != 0xFFFFFFFF;
        }
        if (HMENU sub = GetSubMenu(menu, i)) return Find(sub, labels, at + 1, id);
    }
    return false;
}

int wmain(int argc, wchar_t** argv) {
    if (argc < 3) {
        fwprintf(stderr, L"usage: menu_probe <dll> <path> [label...]\n");
        return 2;
    }
    CoInitializeEx(nullptr, COINIT_APARTMENTTHREADED);
    HMODULE dll = LoadLibraryW(argv[1]);
    if (!dll) {
        fwprintf(stderr, L"LoadLibrary failed: %lu\n", GetLastError());
        return 1;
    }
    auto getClass = reinterpret_cast<HRESULT(STDAPICALLTYPE*)(REFCLSID, REFIID, void**)>(
        GetProcAddress(dll, "DllGetClassObject"));
    IClassFactory* factory = nullptr;
    IShellExtInit* init = nullptr;
    IContextMenu* cm = nullptr;
    IShellItem* item = nullptr;
    IDataObject* data = nullptr;
    if (FAILED(getClass(CLSID_SquashoverlayMenu, IID_PPV_ARGS(&factory))) ||
        FAILED(factory->CreateInstance(nullptr, IID_PPV_ARGS(&init))) ||
        FAILED(init->QueryInterface(IID_PPV_ARGS(&cm)))) {
        fwprintf(stderr, L"creating the handler failed\n");
        return 1;
    }
    if (FAILED(SHCreateItemFromParsingName(argv[2], nullptr, IID_PPV_ARGS(&item))) ||
        FAILED(item->BindToHandler(nullptr, BHID_DataObject, IID_PPV_ARGS(&data)))) {
        fwprintf(stderr, L"no shell data object for %s\n", argv[2]);
        return 1;
    }
    if (FAILED(init->Initialize(nullptr, data, nullptr))) {
        wprintf(L"(no menu for this item)\n");
        return 0;
    }
    HMENU menu = CreatePopupMenu();
    const UINT first = 100;
    HRESULT hr = cm->QueryContextMenu(menu, 0, first, 0x7FFF, CMF_NORMAL);
    if (FAILED(hr)) {
        fwprintf(stderr, L"QueryContextMenu failed: 0x%08lx\n", hr);
        return 1;
    }
    if (argc == 3) {
        Print(menu, 0, first);
        return 0;
    }
    std::vector<std::wstring> labels(argv + 3, argv + argc);
    UINT id = 0;
    if (!Find(menu, labels, 0, &id)) {
        fwprintf(stderr, L"menu item not found\n");
        return 1;
    }
    CMINVOKECOMMANDINFO ici = {sizeof(ici)};
    ici.lpVerb = MAKEINTRESOURCEA(id - first);
    ici.nShow = SW_SHOWNORMAL;
    hr = cm->InvokeCommand(&ici);
    wprintf(L"InvokeCommand: 0x%08lx\n", hr);
    return FAILED(hr) ? 1 : 0;
}
