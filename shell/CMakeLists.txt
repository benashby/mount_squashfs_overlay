cmake_minimum_required(VERSION 3.24)
project(squashoverlay_shell CXX)

set(CMAKE_CXX_STANDARD 20)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
# Static CRT so the DLL loads into Explorer without a VC++ runtime install.
set(CMAKE_MSVC_RUNTIME_LIBRARY "MultiThreaded$<$<CONFIG:Debug>:Debug>")
add_compile_options(/W4 /permissive- /utf-8)
add_compile_definitions(UNICODE _UNICODE WIN32_LEAN_AND_MEAN NOMINMAX)

add_library(squashoverlay_shell SHARED squashoverlay_shell.cpp squashoverlay_shell.def)
target_link_libraries(squashoverlay_shell PRIVATE shell32 ole32 uuid)

add_executable(menu_probe menu_probe.cpp)
target_link_libraries(menu_probe PRIVATE shell32 ole32 uuid)
